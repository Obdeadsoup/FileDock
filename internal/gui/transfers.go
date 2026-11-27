package gui

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"example.com/filedock/internal/syncclient"
)

var errPausedAfterChunks = errors.New("paused after requested chunk count")

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
	ownerSessionID string
	ownerUserID    string

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

func (m *jobManager) getForOwner(jobID, guiSessionID, remoteUserID string) (*uploadJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[jobID]
	if !ok {
		return nil, false
	}
	if j.ownerSessionID != guiSessionID || j.ownerUserID != remoteUserID {
		return nil, false
	}
	return j, true
}

func (m *jobManager) cancelAndCleanForOwner(guiSessionID, remoteUserID string) {
	m.mu.Lock()
	var owned []*uploadJob
	for id, j := range m.jobs {
		if j.ownerSessionID == guiSessionID && (remoteUserID == "" || j.ownerUserID == remoteUserID) {
			owned = append(owned, j)
			delete(m.jobs, id)
		}
	}
	m.mu.Unlock()

	for _, j := range owned {
		j.cancel()
		<-j.done
	}
}

// startResumableUpload opens the local file safely inside os.Root and delegates all chunked
// upload protocol logic (hashing, session create/resume, source mismatch check, skipping confirmed
// chunks, and idempotent completion) to syncclient.Upload.
func (m *jobManager) startResumableUpload(
	guiSessionID string,
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

	api, ownerUserID, err := remote.syncAPI()
	if err != nil {
		_ = f.Close()
		return UploadJobSnapshot{}, err
	}

	fileSize := info.Size()
	fileName := baseRelName(cleanedPath)
	jobID := randomHex(8)
	runCtx, cancel := context.WithCancel(context.Background())

	job := &uploadJob{
		ownerSessionID: guiSessionID,
		ownerUserID:    ownerUserID,
		snap: UploadJobSnapshot{
			JobID:     jobID,
			LocalPath: cleanedPath,
			FileName:  fileName,
			TotalSize: fileSize,
			Status:    "running",
			Resumed:   resumeSessionID != "",
		},
		cancel: cancel,
		done:   make(chan struct{}),
	}

	readyCh := make(chan error, 1)
	readySent := false
	signalReady := func(err error) {
		if !readySent {
			readySent = true
			readyCh <- err
		}
	}

	m.mu.Lock()
	m.jobs[jobID] = job
	m.mu.Unlock()

	go func() {
		defer close(job.done)
		defer f.Close()

		sentThisRun := 0
		res, upErr := syncclient.Upload(runCtx, syncclient.UploadConfig{
			API:       api,
			Source:    f,
			FileName:  fileName,
			FileSize:  fileSize,
			ParentID:  parentID,
			SessionID: resumeSessionID,
			OnSessionReady: func(session syncclient.UploadSessionInfo) {
				job.mu.Lock()
				job.snap.SessionID = session.ID
				job.snap.ChunkSize = session.ChunkSize
				job.snap.TotalChunks = session.TotalChunks
				job.snap.ConfirmedChunks = len(session.UploadedChunks)
				job.snap.SHA256 = session.SHA256
				job.mu.Unlock()
				signalReady(nil)
			},
			OnProgress: func(p syncclient.UploadProgress) {
				job.mu.Lock()
				job.snap.SessionID = p.SessionID
				job.snap.ChunkSize = p.ChunkSize
				job.snap.TotalChunks = p.TotalChunks
				job.snap.ConfirmedChunks = p.ConfirmedChunks
				job.snap.ChunksSent = p.ChunksSent
				job.snap.ChunksSkipped = p.ChunksSkipped
				job.snap.BytesSent = p.BytesSent
				if p.SHA256 != "" {
					job.snap.SHA256 = p.SHA256
				}
				job.mu.Unlock()
			},
			AfterChunkHook: func(_ int) error {
				sentThisRun++
				if stopAfterChunks > 0 && sentThisRun >= stopAfterChunks {
					return errPausedAfterChunks
				}
				return nil
			},
		})

		if upErr != nil {
			if !readySent {
				// Failed before session was ready (e.g., source mismatch, 404, 413): remove job and return error synchronously.
				m.mu.Lock()
				delete(m.jobs, jobID)
				m.mu.Unlock()
				signalReady(upErr)
				return
			}
			job.mu.Lock()
			if errors.Is(upErr, errPausedAfterChunks) {
				job.snap.Status = "canceled"
				job.snap.Error = fmt.Sprintf("paused after %d chunk(s); resume with session %s", sentThisRun, job.snap.SessionID)
			} else if errors.Is(upErr, context.Canceled) || runCtx.Err() != nil {
				job.snap.Status = "canceled"
				job.snap.Error = "upload paused/interrupted by user"
			} else {
				job.snap.Status = "failed"
				job.snap.Error = upErr.Error()
			}
			job.mu.Unlock()
			return
		}

		signalReady(nil)
		job.mu.Lock()
		job.snap.Status = "completed"
		job.snap.FileID = res.File.ID
		job.snap.SHA256 = res.File.SHA256
		job.snap.ChunksSent = res.ChunksSent
		job.snap.ChunksSkipped = res.ChunksSkipped
		job.snap.BytesSent = res.BytesSent
		job.mu.Unlock()
	}()

	if initErr := <-readyCh; initErr != nil {
		return UploadJobSnapshot{}, initErr
	}
	return job.snapshot(), nil
}
