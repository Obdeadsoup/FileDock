package config

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	defaultAddr    = "127.0.0.1:8080"
	defaultDataDir = "./data"
)

type Config struct {
	Addr              string
	DataDir           string
	TLSCertFile       string
	TLSKeyFile        string
	EncryptionKeyFile string
}

// TLSEnabled reports whether TLS certificate and key paths are configured.
func (c Config) TLSEnabled() bool {
	return c.TLSCertFile != "" || c.TLSKeyFile != ""
}

// LoadTLSConfig loads and validates the configured TLS certificate and private key.
// Returns (nil, nil) when TLS is not enabled.
func (c Config) LoadTLSConfig() (*tls.Config, error) {
	if !c.TLSEnabled() {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(c.TLSCertFile, c.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair (cert=%q, key=%q): %w", c.TLSCertFile, c.TLSKeyFile, err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}, nil
}

// IsLoopbackAddr reports whether a host:port listen address is bound strictly to a loopback host.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return IsLoopbackHost(host)
}

// IsLoopbackHost reports whether host is "localhost" or a loopback IP (127.0.0.0/8, ::1).
func IsLoopbackHost(host string) bool {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return false
	}
	if strings.EqualFold(trimmed, "localhost") {
		return true
	}
	ip := net.ParseIP(trimmed)
	return ip != nil && ip.IsLoopback()
}

func Load() (Config, error) {
	addr, set := os.LookupEnv("FILEDOCK_ADDR")
	if !set {
		addr = defaultAddr
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return Config{}, fmt.Errorf("invalid FILEDOCK_ADDR: %w", err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return Config{}, fmt.Errorf("invalid FILEDOCK_ADDR %q: port must be a number from 0 to 65535", addr)
	}

	dataDir, set := os.LookupEnv("FILEDOCK_DATA_DIR")
	if !set {
		dataDir = defaultDataDir
	}
	if strings.TrimSpace(dataDir) == "" {
		return Config{}, fmt.Errorf("invalid FILEDOCK_DATA_DIR: must not be empty")
	}

	certFile := strings.TrimSpace(os.Getenv("FILEDOCK_TLS_CERT_FILE"))
	keyFile := strings.TrimSpace(os.Getenv("FILEDOCK_TLS_KEY_FILE"))
	if (certFile == "") != (keyFile == "") {
		return Config{}, fmt.Errorf("invalid TLS config: both FILEDOCK_TLS_CERT_FILE and FILEDOCK_TLS_KEY_FILE must be set together")
	}
	if !IsLoopbackHost(host) && certFile == "" {
		return Config{}, fmt.Errorf("TLS is required when listening on non-loopback address %q: set FILEDOCK_TLS_CERT_FILE and FILEDOCK_TLS_KEY_FILE", addr)
	}

	encKeyFile := strings.TrimSpace(os.Getenv("FILEDOCK_ENCRYPTION_KEY_FILE"))
	cleanDataDir := filepath.Clean(dataDir)
	if encKeyFile != "" {
		cleanKeyFile := filepath.Clean(encKeyFile)
		if inside, err := isPathInsideDir(cleanDataDir, cleanKeyFile); err != nil {
			return Config{}, fmt.Errorf("validate FILEDOCK_ENCRYPTION_KEY_FILE: %w", err)
		} else if inside {
			return Config{}, fmt.Errorf("invalid FILEDOCK_ENCRYPTION_KEY_FILE %q: key file must be stored outside FILEDOCK_DATA_DIR %q", encKeyFile, cleanDataDir)
		}
		encKeyFile = cleanKeyFile
	}

	return Config{
		Addr:              addr,
		DataDir:           cleanDataDir,
		TLSCertFile:       certFile,
		TLSKeyFile:        keyFile,
		EncryptionKeyFile: encKeyFile,
	}, nil
}

func isPathInsideDir(dir, target string) (bool, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	if resolvedDir, err := filepath.EvalSymlinks(absDir); err == nil {
		absDir = resolvedDir
	}
	if resolvedTarget, err := filepath.EvalSymlinks(absTarget); err == nil {
		absTarget = resolvedTarget
	} else if resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absTarget)); err == nil {
		absTarget = filepath.Join(resolvedParent, filepath.Base(absTarget))
	}
	rel, err := filepath.Rel(absDir, absTarget)
	if err != nil {
		return false, err
	}
	if rel == "." {
		return true, nil
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}
