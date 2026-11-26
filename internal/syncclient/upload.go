package syncclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidUploadFile    = errors.New("invalid local file for upload")
	ErrUploadSourceMismatch = errors.New("local source file does not match upload session size or sha256")
)

// UploadConfig configures a chunked, resumable file upload.
type UploadConfig struct {
	ServerURL      string
	Token          string
	FilePath       string
	ParentID       *string
	SessionID      string
	CACertFile     string
	HTTPClient     *http.Client
	API            API
	OnSessionReady func(session UploadSessionInfo)
	AfterChunkHook func(index int) error
}

// UploadResult summarizes a completed (or partially executed) chunked upload.
type UploadResult struct {
	SessionID     string
	File          FileMeta
	TotalChunks   int
	ChunksSent    int
	ChunksSkipped int
	BytesSent     int64
	Resumed       bool
}

// Upload uploads a local regular file using chunked upload sessions with resume support.
func Upload(ctx context.Context, cfg UploadConfig) (UploadResult, error) {
	normalizedServer, err := NormalizeServerURL(cfg.ServerURL)
	if err != nil {
		return UploadResult{}, err
	}
	if strings.TrimSpace(cfg.FilePath) == "" {
		return UploadResult{}, fmt.Errorf("%w: --file is required", ErrInvalidUploadFile)
	}

	api := cfg.API
	if api == nil {
		httpClient := cfg.HTTPClient
		if httpClient == nil && strings.TrimSpace(cfg.CACertFile) != "" {
			httpClient, err = NewHTTPClient(cfg.CACertFile)
			if err != nil {
				return UploadResult{}, err
			}
		}
		api, err = NewHTTPAPI(normalizedServer, cfg.Token, httpClient)
		if err != nil {
			return UploadResult{}, err
		}
	}

	f, err := os.Open(cfg.FilePath)
	if err != nil {
		return UploadResult{}, fmt.Errorf("open upload file %q: %w", cfg.FilePath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return UploadResult{}, fmt.Errorf("stat upload file %q: %w", cfg.FilePath, err)
	}
	if !info.Mode().IsRegular() {
		return UploadResult{}, fmt.Errorf("%w: %q is not a regular file", ErrInvalidUploadFile, cfg.FilePath)
	}

	fileSize := info.Size()
	fileName := filepath.Base(cfg.FilePath)
	fileSHA, err := computeReaderSHA256(f)
	if err != nil {
		return UploadResult{}, fmt.Errorf("hash upload file %q: %w", cfg.FilePath, err)
	}

	var (
		session UploadSessionInfo
		resumed bool
	)
	trimmedSession := strings.TrimSpace(cfg.SessionID)
	if trimmedSession == "" {
		session, err = api.CreateUploadSession(ctx, cfg.ParentID, fileName, fileSize, fileSHA)
		if err != nil {
			return UploadResult{}, err
		}
	} else {
		resumed = true
		session, err = api.GetUploadSession(ctx, trimmedSession)
		if err != nil {
			return UploadResult{}, err
		}
		if session.Size != fileSize || session.SHA256 != fileSHA {
			return UploadResult{}, fmt.Errorf(
				"%w: local size=%d sha256=%s, session size=%d sha256=%s",
				ErrUploadSourceMismatch, fileSize, fileSHA, session.Size, session.SHA256,
			)
		}
	}

	if cfg.OnSessionReady != nil {
		cfg.OnSessionReady(session)
	}

	res := UploadResult{
		SessionID:   session.ID,
		TotalChunks: session.TotalChunks,
		Resumed:     resumed,
	}

	if session.Status == "completed" {
		res.ChunksSkipped = session.TotalChunks
		published, err := api.CompleteUploadSession(ctx, session.ID)
		if err != nil {
			return res, err
		}
		res.File = published
		return res, nil
	}

	confirmed := make(map[int]bool, len(session.UploadedChunks))
	for _, idx := range session.UploadedChunks {
		confirmed[idx] = true
	}

	for idx := 0; idx < session.TotalChunks; idx++ {
		if confirmed[idx] {
			res.ChunksSkipped++
			continue
		}
		offset := int64(idx) * session.ChunkSize
		chunkLen := session.ChunkSize
		if remaining := fileSize - offset; remaining < chunkLen {
			chunkLen = remaining
		}

		chunkSHA, err := computeReaderSHA256(io.NewSectionReader(f, offset, chunkLen))
		if err != nil {
			return res, fmt.Errorf("hash chunk %d: %w", idx, err)
		}
		if err := api.UploadChunk(ctx, session.ID, idx, chunkSHA, io.NewSectionReader(f, offset, chunkLen), chunkLen); err != nil {
			return res, fmt.Errorf("upload chunk %d: %w", idx, err)
		}
		res.ChunksSent++
		res.BytesSent += chunkLen
		if cfg.AfterChunkHook != nil {
			if err := cfg.AfterChunkHook(idx); err != nil {
				return res, err
			}
		}
	}

	published, err := api.CompleteUploadSession(ctx, session.ID)
	if err != nil {
		return res, err
	}
	res.File = published
	return res, nil
}

func computeReaderSHA256(r io.Reader) (string, error) {
	h := sha256.New()
	buf := make([]byte, 64*1024)
	if _, err := io.CopyBuffer(h, r, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
