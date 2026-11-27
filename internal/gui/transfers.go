package gui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"time"

	"example.com/filedock/internal/syncclient"
)

type UploadJobSnapshot struct {
	JobID           string `json:"job_id"`
	SessionID       string `json:"session_id"`
	LocalPath       string `json:"local_path"`
	FileName        string `json:"file_name"`
	TotalSize       int64  `json:"total_size"`
	ChunkSize       int64  `json:"chunk_size"`
	TotalChunks     int    `json:"total_chunks"`
	ConfirmedChunks int    `json:"confirmed_chunks"`
	ChunksSent      int    `json:"chunks_sent"`
	ChunksSkipped   int    `json:"chunks_skipped"`
	BytesSent       int64  `json:"bytes_sent"`
	Status          string `json:"status"` // running | canceled | completed | failed
	FileID          string `json:"file_id,omitempty"`
	SHA256          string `json:"sha256,omitempty"`
	Error           string `json:"error,omitempty"`
	Resumed         bool   `json:"resumed"`
}

type uploadJob struct {
	mu     sync.Mutex
	snap   UploadJobSnapshot
	cancel context.CancelFunc
	done   chan struct{}
}

func (j *uploadJob) snapshot() UploadJobSnapshot {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.snap
}

type jobManager struct {
	mu   sync.Mutex
	jobs map[string]*uploadJob
}

func newJobManager() *jobManager {
	return &jobManager{jobs: make(map[string]*uploadJob)}
}

func (m *jobManager) get(jobID string) (*uploadJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[jobID]
	return j, ok
}

func (m *jobManager) startResumableUpload(
	localDir string,
	remote *remoteClient,
	localRelPath string,
	parentID *string,
	resumeSessionID string,
	stopAfterChunks int,
) (UploadJobSnapshot, error) {
	f, info, cleanedPath, err := openLocalRegularFile(localDir, localRelPath)
	if err != nil {
		return UploadJobSnapshot{}, err
	}

	fileSize := info.Size()
	fileName := path.Base(cleanedPath)
	fileSHA, err := hashFileStream(f)
	if err != nil {
		_ = f.Close()
		return UploadJobSnapshot{}, err
	}

	api, err := remote.syncAPI()
	if err != nil {
		_ = f.Close()
		return UploadJobSnapshot{}, err
	}

	initCtx, cancelInit := context.WithTimeout(context.Background(), 30*time.Second)
	var (
		session syncclient.UploadSessionInfo
		resumed bool
	)
	if resumeSessionID == "" {
		session, err = api.CreateUploadSession(initCtx, parentID, fileName, fileSize, fileSHA)
	} else {
		resumed = true
		session, err = api.GetUploadSession(initCtx, resumeSessionID)
		if err == nil && (session.Size != fileSize || session.SHA256 != fileSHA) {
			err = fmt.Errorf("%w: local size=%d sha=%s, session size=%d sha=%s",
				syncclient.ErrUploadSourceMismatch, fileSize, fileSHA, session.Size, session.SHA256)
		}
	}
	cancelInit()
	if err != nil {
		_ = f.Close()
		return UploadJobSnapshot{}, err
	}

	jobID := randomHex(8)
	runCtx, cancel := context.WithCancel(context.Background())
	job := &uploadJob{
		snap: UploadJobSnapshot{
			JobID:           jobID,
			SessionID:       session.ID,
			LocalPath:       cleanedPath,
			FileName:        fileName,
			TotalSize:       fileSize,
			ChunkSize:       session.ChunkSize,
			TotalChunks:     session.TotalChunks,
			ConfirmedChunks: len(session.UploadedChunks),
			Status:          "running",
			SHA256:          fileSHA,
			Resumed:         resumed,
		},
		cancel: cancel,
		done:   make(chan struct{}),
	}

	m.mu.Lock()
	m.jobs[jobID] = job
	m.mu.Unlock()

	go runUploadJob(runCtx, job, f, api, session, fileSize, stopAfterChunks)

	return job.snapshot(), nil
}

func runUploadJob(
	ctx context.Context,
	job *uploadJob,
	f *os.File,
	api *syncclient.HTTPAPI,
	session syncclient.UploadSessionInfo,
	fileSize int64,
	stopAfterChunks int,
) {
	defer close(job.done)
	defer f.Close()

	if session.Status == "completed" {
		published, err := api.CompleteUploadSession(ctx, session.ID)
		job.mu.Lock()
		if err != nil {
			job.snap.Status = "failed"
			job.snap.Error = err.Error()
		} else {
			job.snap.Status = "completed"
			job.snap.ChunksSkipped = session.TotalChunks
			job.snap.ConfirmedChunks = session.TotalChunks
			job.snap.FileID = published.ID
			job.snap.SHA256 = published.SHA256
		}
		job.mu.Unlock()
		return
	}

	confirmed := make(map[int]bool, len(session.UploadedChunks))
	for _, idx := range session.UploadedChunks {
		confirmed[idx] = true
	}

	sentThisRun := 0
	for idx := 0; idx < session.TotalChunks; idx++ {
		if ctx.Err() != nil {
			job.mu.Lock()
			job.snap.Status = "canceled"
			job.snap.Error = "upload paused/interrupted by user"
			job.mu.Unlock()
			return
		}
		if confirmed[idx] {
			job.mu.Lock()
			job.snap.ChunksSkipped++
			job.mu.Unlock()
			continue
		}

		offset := int64(idx) * session.ChunkSize
		chunkLen := session.ChunkSize
		if rem := fileSize - offset; rem < chunkLen {
			chunkLen = rem
		}
		chunkSHA, err := hashReaderStream(io.NewSectionReader(f, offset, chunkLen))
		if err != nil {
			job.mu.Lock()
			job.snap.Status = "failed"
			job.snap.Error = err.Error()
			job.mu.Unlock()
			return
		}
		if err := api.UploadChunk(ctx, session.ID, idx, chunkSHA, io.NewSectionReader(f, offset, chunkLen), chunkLen); err != nil {
			job.mu.Lock()
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				job.snap.Status = "canceled"
				job.snap.Error = "upload paused/interrupted by user"
			} else {
				job.snap.Status = "failed"
				job.snap.Error = err.Error()
			}
			job.mu.Unlock()
			return
		}

		sentThisRun++
		job.mu.Lock()
		job.snap.ChunksSent++
		job.snap.ConfirmedChunks++
		job.snap.BytesSent += chunkLen
		job.mu.Unlock()

		if stopAfterChunks > 0 && sentThisRun >= stopAfterChunks {
			job.mu.Lock()
			job.snap.Status = "canceled"
			job.snap.Error = fmt.Sprintf("paused after %d chunk(s); resume with session %s", sentThisRun, session.ID)
			job.mu.Unlock()
			return
		}
	}

	published, err := api.CompleteUploadSession(ctx, session.ID)
	job.mu.Lock()
	if err != nil {
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			job.snap.Status = "canceled"
			job.snap.Error = "upload canceled before completion"
		} else {
			job.snap.Status = "failed"
			job.snap.Error = err.Error()
		}
	} else {
		job.snap.Status = "completed"
		job.snap.FileID = published.ID
		job.snap.SHA256 = published.SHA256
	}
	job.mu.Unlock()
}

func hashFileStream(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hashReaderStream(f)
}

func hashReaderStream(r io.Reader) (string, error) {
	h := sha256.New()
	buf := make([]byte, 64*1024)
	if _, err := io.CopyBuffer(h, r, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
