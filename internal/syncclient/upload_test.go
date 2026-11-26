package syncclient_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"example.com/filedock/internal/syncclient"
)

func TestUploadInterruptResumeAndSourceMismatch(t *testing.T) {
	ts := startTestServer(t)
	ctx := context.Background()

	// Create a 10 MiB local file (3 chunks with default 4 MiB chunk size: 4 MiB, 4 MiB, 2 MiB).
	payload := make([]byte, 10<<20)
	for i := range payload {
		payload[i] = byte((i*31 + 7) % 251)
	}
	wantSum := sha256.Sum256(payload)
	wantSHA := hex.EncodeToString(wantSum[:])

	localFile := filepath.Join(t.TempDir(), "bundle.bin")
	if err := os.WriteFile(localFile, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Start upload, capture session ID before transfer, and interrupt after chunk 0 (4 MiB).
	var sessionID string
	errInterrupt := errors.New("simulated client disconnect")
	res1, err := syncclient.Upload(ctx, syncclient.UploadConfig{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		FilePath:  localFile,
		OnSessionReady: func(s syncclient.UploadSessionInfo) {
			sessionID = s.ID
		},
		AfterChunkHook: func(index int) error {
			if index == 0 {
				return errInterrupt
			}
			return nil
		},
	})
	if !errors.Is(err, errInterrupt) {
		t.Fatalf("interrupted Upload() error = %v, want %v", err, errInterrupt)
	}
	if sessionID == "" || res1.ChunksSent != 1 || res1.BytesSent != 4<<20 {
		t.Fatalf("interrupted Upload() session=%q res=%+v, want 1 chunk (4 MiB)", sessionID, res1)
	}

	// 2. Trying to resume with a modified local file must fail before sending any chunks.
	wrongFile := filepath.Join(t.TempDir(), "bundle.bin")
	if err := os.WriteFile(wrongFile, []byte("mutated content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := syncclient.Upload(ctx, syncclient.UploadConfig{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		FilePath:  wrongFile,
		SessionID: sessionID,
	}); !errors.Is(err, syncclient.ErrUploadSourceMismatch) {
		t.Fatalf("resume with wrong file error = %v, want ErrUploadSourceMismatch", err)
	}

	// 3. Resume with original file -> only sends remaining 2 chunks (6 MiB).
	res2, err := syncclient.Upload(ctx, syncclient.UploadConfig{
		ServerURL: ts.srv.URL,
		Token:     ts.tokenA,
		FilePath:  localFile,
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("resumed Upload() error = %v", err)
	}
	if !res2.Resumed || res2.ChunksSkipped != 1 || res2.ChunksSent != 2 || res2.BytesSent != 6<<20 {
		t.Fatalf("resumed Upload() = %+v, want Resumed=true skipped=1 sent=2 bytes=6MiB", res2)
	}
	if res2.File.ID == "" || res2.File.SHA256 != wantSHA || res2.File.Size != int64(len(payload)) {
		t.Fatalf("published file = %+v, want sha=%s size=%d", res2.File, wantSHA, len(payload))
	}

	// 4. Download the published file and verify exact bytes.
	httpAPI, err := syncclient.NewHTTPAPI(ts.srv.URL, ts.tokenA, nil)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := httpAPI.DownloadFile(ctx, res2.File.ID)
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	downloaded, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(downloaded, payload) {
		t.Fatalf("downloaded bytes differ from uploaded payload")
	}
}
