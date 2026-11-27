package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	unsetEnvForTest(t, "FILEDOCK_ADDR")
	unsetEnvForTest(t, "FILEDOCK_DATA_DIR")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != defaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, defaultAddr)
	}
	if cfg.DataDir != "data" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "data")
	}
}

func TestLoadCustomValues(t *testing.T) {
	t.Setenv("FILEDOCK_ADDR", "127.0.0.1:9090")
	t.Setenv("FILEDOCK_DATA_DIR", "/tmp/filedock-test/")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != "127.0.0.1:9090" {
		t.Errorf("Addr = %q, want %q", cfg.Addr, "127.0.0.1:9090")
	}
	if want := filepath.FromSlash("/tmp/filedock-test"); cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
}

func TestLoadRejectsInvalidDataDir(t *testing.T) {
	t.Setenv("FILEDOCK_DATA_DIR", "   ")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for blank FILEDOCK_DATA_DIR, got nil")
	}
}

func TestLoadRequiresTLSForNonLoopback(t *testing.T) {
	unsetEnvForTest(t, "FILEDOCK_TLS_CERT_FILE")
	unsetEnvForTest(t, "FILEDOCK_TLS_KEY_FILE")

	for _, addr := range []string{"0.0.0.0:8443", ":8443", "[::]:8443", "192.168.1.10:8443"} {
		t.Setenv("FILEDOCK_ADDR", addr)
		if _, err := Load(); err == nil {
			t.Fatalf("Load() for non-loopback %q without TLS: expected error, got nil", addr)
		}
	}

	// Partial TLS config (cert without key) is rejected even on loopback.
	t.Setenv("FILEDOCK_ADDR", "127.0.0.1:8443")
	t.Setenv("FILEDOCK_TLS_CERT_FILE", "/tmp/cert.pem")
	unsetEnvForTest(t, "FILEDOCK_TLS_KEY_FILE")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error when only FILEDOCK_TLS_CERT_FILE is set")
	}

	// Non-loopback with both cert and key paths passes config.Load, and LoadTLSConfig fails cleanly on missing files.
	t.Setenv("FILEDOCK_ADDR", "0.0.0.0:8443")
	t.Setenv("FILEDOCK_TLS_CERT_FILE", "/tmp/does-not-exist-cert.pem")
	t.Setenv("FILEDOCK_TLS_KEY_FILE", "/tmp/does-not-exist-key.pem")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with TLS paths error = %v", err)
	}
	if !cfg.TLSEnabled() {
		t.Fatal("expected TLSEnabled() == true")
	}
	if _, err := cfg.LoadTLSConfig(); err == nil {
		t.Fatal("LoadTLSConfig() expected error for missing cert/key files")
	}
}

func TestLoadRejectsEncryptionKeyInsideDataDir(t *testing.T) {
	unsetEnvForTest(t, "FILEDOCK_TLS_CERT_FILE")
	unsetEnvForTest(t, "FILEDOCK_TLS_KEY_FILE")
	t.Setenv("FILEDOCK_ADDR", "127.0.0.1:8080")

	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	t.Setenv("FILEDOCK_DATA_DIR", dataDir)

	// Key inside dataDir must be rejected.
	t.Setenv("FILEDOCK_ENCRYPTION_KEY_FILE", filepath.Join(dataDir, "master.keyset.json"))
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error when FILEDOCK_ENCRYPTION_KEY_FILE is inside FILEDOCK_DATA_DIR")
	}

	// Key outside dataDir is accepted.
	outsideKey := filepath.Join(base, "secrets", "master.keyset.json")
	t.Setenv("FILEDOCK_ENCRYPTION_KEY_FILE", outsideKey)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with outside key error = %v", err)
	}
	if cfg.EncryptionKeyFile != outsideKey {
		t.Errorf("EncryptionKeyFile = %q, want %q", cfg.EncryptionKeyFile, outsideKey)
	}
}

func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("os.Unsetenv(%q): %v", key, err)
	}
}
