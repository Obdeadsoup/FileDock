package syncclient_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"example.com/filedock/internal/syncclient"
)

func TestBearerTransportAndRedirectSecurity(t *testing.T) {
	// 1. Non-loopback HTTP is rejected.
	for _, bad := range []string{"http://example.com:8080", "http://192.168.1.10:8080", "http://10.0.0.5"} {
		if _, err := syncclient.NewHTTPAPI(bad, "secret-token", nil); !errors.Is(err, syncclient.ErrInsecureTransport) {
			t.Fatalf("NewHTTPAPI(%q) = %v, want ErrInsecureTransport", bad, err)
		}
	}
	// Loopback HTTP and any HTTPS URL are accepted.
	for _, good := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080", "https://files.example.com"} {
		if _, err := syncclient.NewHTTPAPI(good, "secret-token", nil); err != nil {
			t.Fatalf("NewHTTPAPI(%q) unexpected error: %v", good, err)
		}
	}

	// 2. Cross-origin redirects (302, 307, 308) must be rejected without forwarding token OR body!
	var targetHits atomic.Int32
	var receivedAuth string
	var receivedBody []byte
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		receivedAuth = r.Header.Get("Authorization")
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer targetSrv.Close()

	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetSrv.URL+r.URL.Path, status)
		}))
		api, err := syncclient.NewHTTPAPI(redirectSrv.URL, "top-secret-token", nil)
		if err != nil {
			redirectSrv.Close()
			t.Fatal(err)
		}
		if status == http.StatusFound {
			_, err = api.Me(context.Background())
		} else {
			secretBody := []byte("confidential-upload-body")
			_, _, err = api.ReplaceFileContent(context.Background(), "file-1", `"rev-1"`, bytes.NewReader(secretBody), int64(len(secretBody)))
		}
		redirectSrv.Close()
		if !errors.Is(err, syncclient.ErrCrossOriginRedirect) {
			t.Fatalf("status %d cross-origin redirect error = %v, want ErrCrossOriginRedirect", status, err)
		}
	}
	if targetHits.Load() != 0 || receivedAuth != "" || len(receivedBody) != 0 {
		t.Fatalf("cross-origin target received hits=%d auth=%q body=%q, want 0/empty", targetHits.Load(), receivedAuth, string(receivedBody))
	}

	// 3. Redirect to non-loopback HTTP must be blocked.
	badRedirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/steal", http.StatusFound)
	}))
	defer badRedirectSrv.Close()

	api2, err := syncclient.NewHTTPAPI(badRedirectSrv.URL, "top-secret-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api2.Me(context.Background()); !errors.Is(err, syncclient.ErrInsecureTransport) {
		t.Fatalf("redirect to non-loopback HTTP error = %v, want ErrInsecureTransport", err)
	}
}

func TestHTTPSClientTrustedUntrustedAndHostnameMismatch(t *testing.T) {
	caPEM, serverCert := generateTestCAAndCert(t, []string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")})
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u1","username":"alice"}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}}
	srv.StartTLS()
	defer srv.Close()

	// 1. Default client without custom CA must fail (untrusted cert).
	defaultClient, err := syncclient.NewHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	untrustedAPI, err := syncclient.NewHTTPAPI(srv.URL, "tok", defaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrustedAPI.Me(context.Background()); err == nil {
		t.Fatal("expected untrusted CA certificate to fail verification")
	}

	// 2. Client with explicit local CA succeeds on matching IP/hostname.
	trustedClient, err := syncclient.NewHTTPClient(caFile)
	if err != nil {
		t.Fatal(err)
	}
	trustedAPI, err := syncclient.NewHTTPAPI(srv.URL, "tok", trustedClient)
	if err != nil {
		t.Fatal(err)
	}
	me, err := trustedAPI.Me(context.Background())
	if err != nil || me.ID != "u1" {
		t.Fatalf("trusted HTTPS Me() = (%+v, %v), want u1", me, err)
	}

	// 3. Hostname mismatch must fail even with the trusted CA.
	_, wrongHostCert := generateTestCAAndCertWithSigner(t, caPEM, nil)
	_ = wrongHostCert
	mismatchCA, mismatchCert := generateTestCAAndCert(t, []string{"other.invalid"}, nil)
	mismatchCAFile := filepath.Join(t.TempDir(), "mismatch-ca.pem")
	if err := os.WriteFile(mismatchCAFile, mismatchCA, 0o644); err != nil {
		t.Fatal(err)
	}
	mismatchSrv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mismatchSrv.TLS = &tls.Config{Certificates: []tls.Certificate{mismatchCert}}
	mismatchSrv.StartTLS()
	defer mismatchSrv.Close()

	mismatchClient, err := syncclient.NewHTTPClient(mismatchCAFile)
	if err != nil {
		t.Fatal(err)
	}
	mismatchAPI, err := syncclient.NewHTTPAPI(mismatchSrv.URL, "tok", mismatchClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mismatchAPI.Me(context.Background()); err == nil {
		t.Fatal("expected hostname mismatch to fail TLS verification")
	}
}

func generateTestCAAndCertWithSigner(t *testing.T, _ []byte, _ any) ([]byte, tls.Certificate) {
	t.Helper()
	return nil, tls.Certificate{}
}

func generateTestCAAndCert(t *testing.T, dnsNames []string, ips []net.IP) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "FileDock Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "FileDock Test Server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srvKeyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatal(err)
	}
	srvCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})
	srvKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: srvKeyDER})
	tlsCert, err := tls.X509KeyPair(srvCertPEM, srvKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return caPEM, tlsCert
}
