package config

import (
	"os"
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
	if cfg.DataDir != "/tmp/filedock-test" {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, "/tmp/filedock-test")
	}
}

func TestLoadRejectsInvalidDataDir(t *testing.T) {
	t.Setenv("FILEDOCK_DATA_DIR", "   ")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected error for blank FILEDOCK_DATA_DIR, got nil")
	}
}

func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("os.Unsetenv(%q): %v", key, err)
	}
}
