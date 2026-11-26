package main

import (
	"bytes"
	"context"
	"testing"
)

func TestCLIValidation(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{}, &out); err == nil {
		t.Fatal("expected usage error when no args supplied")
	}
	t.Setenv("FILEDOCK_TOKEN", "")
	if err := run(context.Background(), []string{"sync", "pull", "--server", "http://127.0.0.1:8080", "--dir", t.TempDir()}, &out); err == nil {
		t.Fatal("expected error when FILEDOCK_TOKEN is empty for pull")
	}
	if err := run(context.Background(), []string{"sync", "push", "--server", "http://127.0.0.1:8080", "--dir", t.TempDir(), "--path", "a.txt"}, &out); err == nil {
		t.Fatal("expected error when FILEDOCK_TOKEN is empty for push")
	}
	if err := run(context.Background(), []string{"upload", "--server", "http://127.0.0.1:8080", "--file", "/tmp/nonexistent"}, &out); err == nil {
		t.Fatal("expected error when FILEDOCK_TOKEN is empty for upload")
	}
}
