package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"example.com/filedock/internal/files"
)

const uploadSessionsSchemaDDL = `
CREATE TABLE IF NOT EXISTS upload_sessions (
	id TEXT PRIMARY KEY,
	owner_id TEXT NOT NULL,
	parent_id TEXT,
	name TEXT NOT NULL,
	total_size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	chunk_size INTEGER NOT NULL,
	total_chunks INTEGER NOT NULL,
	status TEXT NOT NULL CHECK (status IN ('open', 'completed')),
	file_id TEXT,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL,
	expires_at TEXT NOT NULL,
	expires_at_ns INTEGER NOT NULL,
	completed_at TEXT,
	completed_at_ns INTEGER
);

CREATE INDEX IF NOT EXISTS idx_upload_sessions_owner_status
	ON upload_sessions (owner_id, status, expires_at_ns);

CREATE INDEX IF NOT EXISTS idx_upload_sessions_expires
	ON upload_sessions (status, expires_at_ns);

CREATE TABLE IF NOT EXISTS upload_chunks (
	session_id TEXT NOT NULL,
	chunk_index INTEGER NOT NULL,
	size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	storage_key TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	PRIMARY KEY (session_id, chunk_index),
	FOREIGN KEY (session_id) REFERENCES upload_sessions(id) ON DELETE CASCADE
);
`

func migrateUploadSessions(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, uploadSessionsSchemaDDL); err != nil {
		return fmt.Errorf("initialize upload_sessions schema: %w", err)
	}
	hasStorageKey, err := tableHasColumn(ctx, tx, "upload_chunks", "storage_key")
	if err != nil {
		return err
	}
	if !hasStorageKey {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE upload_chunks ADD COLUMN storage_key TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add storage_key column to upload_chunks: %w", err)
		}
	}
	return nil
}

// CreateUploadSession validates destination parent ownership and active staging quotas,
// then persists a new open upload session in a single transaction.
func (s *Store) CreateUploadSession(ctx context.Context, session files.UploadSession, maxActiveSessions int, maxStagingBytes int64, now time.Time) error {
	if strings.TrimSpace(session.OwnerID) == "" {
		return files.ErrInvalidOwner
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create upload session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := checkParent(ctx, tx, session.OwnerID, session.ParentID); err != nil {
		return err
	}

	nowNS := now.UTC().UnixNano()
	var (
		activeCount int
		activeBytes int64
	)
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*), COALESCE(SUM(total_size), 0)
		 FROM upload_sessions
		 WHERE owner_id = ? AND status = 'open' AND expires_at_ns > ?`,
		session.OwnerID,
		nowNS,
	).Scan(&activeCount, &activeBytes); err != nil {
		return fmt.Errorf("query active upload staging quota: %w", err)
	}
	if maxActiveSessions > 0 && activeCount >= maxActiveSessions {
		return fmt.Errorf("%w: active upload sessions limit (%d) reached", files.ErrUploadQuotaExceeded, maxActiveSessions)
	}
	if maxStagingBytes > 0 && activeBytes+session.Size > maxStagingBytes {
		return fmt.Errorf("%w: active upload staging bytes limit (%d) exceeded", files.ErrUploadQuotaExceeded, maxStagingBytes)
	}

	createdUTC := session.CreatedAt.UTC()
	expiresUTC := session.ExpiresAt.UTC()
	const insertQuery = `
INSERT INTO upload_sessions (
	id, owner_id, parent_id, name, total_size, sha256, chunk_size, total_chunks,
	status, created_at, created_at_ns, expires_at, expires_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(
		ctx,
		insertQuery,
		session.ID,
		session.OwnerID,
		session.ParentID,
		session.Name,
		session.Size,
		session.SHA256,
		session.ChunkSize,
		session.TotalChunks,
		files.UploadStatusOpen,
		createdUTC.Format(time.RFC3339Nano),
		createdUTC.UnixNano(),
		expiresUTC.Format(time.RFC3339Nano),
		expiresUTC.UnixNano(),
	); err != nil {
		return fmt.Errorf("insert upload session: %w", err)
	}
	return tx.Commit()
}

// GetUploadSession fetches an owner-scoped upload session and its confirmed chunks ordered by chunk_index ASC.
func (s *Store) GetUploadSession(ctx context.Context, ownerID, sessionID string) (files.UploadSession, []files.UploadChunk, error) {
	if strings.TrimSpace(ownerID) == "" {
		return files.UploadSession{}, nil, files.ErrUploadSessionNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return files.UploadSession{}, nil, fmt.Errorf("begin get upload session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	session, err := getUploadSessionTx(ctx, tx, ownerID, sessionID)
	if err != nil {
		return files.UploadSession{}, nil, err
	}

	rows, err := tx.QueryContext(
		ctx,
		`SELECT chunk_index, size, sha256, storage_key, created_at
		 FROM upload_chunks
		 WHERE session_id = ?
		 ORDER BY chunk_index ASC`,
		sessionID,
	)
	if err != nil {
		return files.UploadSession{}, nil, fmt.Errorf("query upload chunks: %w", err)
	}
	defer rows.Close()

	chunks := make([]files.UploadChunk, 0)
	for rows.Next() {
		var (
			c          files.UploadChunk
			createdStr string
		)
		c.SessionID = sessionID
		if err := rows.Scan(&c.Index, &c.Size, &c.SHA256, &c.StorageKey, &createdStr); err != nil {
			return files.UploadSession{}, nil, fmt.Errorf("scan upload chunk: %w", err)
		}
		parsedAt, err := time.Parse(time.RFC3339Nano, createdStr)
		if err != nil {
			return files.UploadSession{}, nil, fmt.Errorf("parse chunk created_at %q: %w", createdStr, err)
		}
		c.CreatedAt = parsedAt.UTC()
		chunks = append(chunks, c)
	}
	if err := rows.Err(); err != nil {
		return files.UploadSession{}, nil, fmt.Errorf("iterate upload chunks: %w", err)
	}
	return session, chunks, nil
}

// RecordUploadChunk records a confirmed chunk for an open, non-expired session owned by ownerID.
// If the chunk index already exists with identical size and SHA-256, returns (true, nil).
// If it exists with different size or SHA-256, returns (false, files.ErrChunkConflict).
func (s *Store) RecordUploadChunk(ctx context.Context, ownerID string, chunk files.UploadChunk, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin record upload chunk: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	session, err := getUploadSessionTx(ctx, tx, ownerID, chunk.SessionID)
	if err != nil {
		return false, err
	}
	if session.Status == files.UploadStatusCompleted {
		return false, files.ErrUploadSessionClosed
	}
	if !now.UTC().Before(session.ExpiresAt) {
		return false, files.ErrUploadSessionExpired
	}

	var (
		existingSize int64
		existingSHA  string
	)
	err = tx.QueryRowContext(
		ctx,
		`SELECT size, sha256 FROM upload_chunks WHERE session_id = ? AND chunk_index = ?`,
		chunk.SessionID,
		chunk.Index,
	).Scan(&existingSize, &existingSHA)
	if err == nil {
		if existingSize == chunk.Size && existingSHA == chunk.SHA256 {
			return true, nil
		}
		return false, files.ErrChunkConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("check existing upload chunk: %w", err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO upload_chunks (session_id, chunk_index, size, sha256, storage_key, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		chunk.SessionID,
		chunk.Index,
		chunk.Size,
		chunk.SHA256,
		chunk.StorageKey,
		chunk.CreatedAt.UTC().Format(time.RFC3339Nano),
	); err != nil {
		return false, fmt.Errorf("insert upload chunk: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit upload chunk: %w", err)
	}
	return false, nil
}

// CompleteUploadSession atomically publishes the assembled file, appends a single file.upsert event,
// marks the session completed with its final file_id, and deletes its upload_chunks rows.
// If the session was already completed, returns the existing published File with alreadyCompleted=true.
func (s *Store) CompleteUploadSession(ctx context.Context, ownerID, sessionID string, file files.StoredFile, now time.Time) (files.File, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return files.File{}, false, fmt.Errorf("begin complete upload session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	session, err := getUploadSessionTx(ctx, tx, ownerID, sessionID)
	if err != nil {
		return files.File{}, false, err
	}
	if session.Status == files.UploadStatusCompleted && session.FileID != nil {
		createdAt := session.CreatedAt
		if session.CompletedAt != nil {
			createdAt = *session.CompletedAt
		}
		return files.File{
			ID:              *session.FileID,
			Name:            session.Name,
			ParentID:        session.ParentID,
			Size:            session.Size,
			SHA256:          session.SHA256,
			ContentRevision: 1,
			CreatedAt:       createdAt.UTC(),
		}, true, nil
	}
	if !now.UTC().Before(session.ExpiresAt) {
		return files.File{}, false, files.ErrUploadSessionExpired
	}

	if err := checkParent(ctx, tx, ownerID, session.ParentID); err != nil {
		return files.File{}, false, err
	}

	createdUTC := file.CreatedAt.UTC()
	rev := file.ContentRevision
	if rev <= 0 {
		rev = 1
		file.ContentRevision = 1
	}

	const insertFileQuery = `
INSERT INTO files (id, owner_id, parent_id, name, size, sha256, object_key, content_revision, created_at, created_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(
		ctx,
		insertFileQuery,
		file.ID,
		ownerID,
		session.ParentID,
		session.Name,
		file.Size,
		file.SHA256,
		file.ObjectKey,
		rev,
		createdUTC.Format(time.RFC3339Nano),
		createdUTC.UnixNano(),
	); err != nil {
		return files.File{}, false, fmt.Errorf("insert completed upload file: %w", err)
	}

	published := files.File{
		ID:              file.ID,
		Name:            session.Name,
		ParentID:        session.ParentID,
		Size:            file.Size,
		SHA256:          file.SHA256,
		ContentRevision: rev,
		CreatedAt:       createdUTC,
	}
	if err := appendFileUpsert(ctx, tx, ownerID, published, createdUTC); err != nil {
		return files.File{}, false, err
	}
	if err := appendAuditLog(ctx, tx, ownerID, files.AuditActionFileUpload, files.ResourceTypeFile, file.ID, createdUTC); err != nil {
		return files.File{}, false, err
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE upload_sessions
		 SET status = ?, file_id = ?, completed_at = ?, completed_at_ns = ?
		 WHERE id = ? AND owner_id = ?`,
		files.UploadStatusCompleted,
		file.ID,
		createdUTC.Format(time.RFC3339Nano),
		createdUTC.UnixNano(),
		sessionID,
		ownerID,
	); err != nil {
		return files.File{}, false, fmt.Errorf("mark upload session completed: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM upload_chunks WHERE session_id = ?`, sessionID); err != nil {
		return files.File{}, false, fmt.Errorf("delete completed upload chunks: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return files.File{}, false, fmt.Errorf("commit complete upload session: %w", err)
	}
	return published, false, nil
}

// ListExpiredUploadSessions returns up to limit IDs of open upload sessions that have expired.
func (s *Store) ListExpiredUploadSessions(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = files.DefaultUploadCleanupBatchSize
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id FROM upload_sessions
		 WHERE status = 'open' AND expires_at_ns <= ?
		 ORDER BY expires_at_ns ASC, id ASC
		 LIMIT ?`,
		now.UTC().UnixNano(),
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list expired upload sessions: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired upload session id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired upload sessions: %w", err)
	}
	return ids, nil
}

// DeleteExpiredUploadSession removes an expired open upload session and its upload_chunks rows.
func (s *Store) DeleteExpiredUploadSession(ctx context.Context, sessionID string) error {
	if _, err := s.db.ExecContext(
		ctx,
		`DELETE FROM upload_sessions WHERE id = ? AND status = 'open'`,
		sessionID,
	); err != nil {
		return fmt.Errorf("delete expired upload session: %w", err)
	}
	return nil
}

func getUploadSessionTx(ctx context.Context, db rowQuerier, ownerID, sessionID string) (files.UploadSession, error) {
	const query = `
SELECT id, owner_id, parent_id, name, total_size, sha256, chunk_size, total_chunks,
       status, file_id, created_at, expires_at, completed_at
FROM upload_sessions
WHERE id = ? AND owner_id = ?`

	var (
		session      files.UploadSession
		parentNull   sql.NullString
		fileIDNull   sql.NullString
		createdStr   string
		expiresStr   string
		completedStr sql.NullString
	)
	err := db.QueryRowContext(ctx, query, sessionID, ownerID).Scan(
		&session.ID,
		&session.OwnerID,
		&parentNull,
		&session.Name,
		&session.Size,
		&session.SHA256,
		&session.ChunkSize,
		&session.TotalChunks,
		&session.Status,
		&fileIDNull,
		&createdStr,
		&expiresStr,
		&completedStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return files.UploadSession{}, files.ErrUploadSessionNotFound
		}
		return files.UploadSession{}, fmt.Errorf("get upload session: %w", err)
	}
	if parentNull.Valid {
		p := parentNull.String
		session.ParentID = &p
	}
	if fileIDNull.Valid {
		f := fileIDNull.String
		session.FileID = &f
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdStr)
	if err != nil {
		return files.UploadSession{}, fmt.Errorf("parse session created_at %q: %w", createdStr, err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresStr)
	if err != nil {
		return files.UploadSession{}, fmt.Errorf("parse session expires_at %q: %w", expiresStr, err)
	}
	session.CreatedAt = createdAt.UTC()
	session.ExpiresAt = expiresAt.UTC()
	if completedStr.Valid && completedStr.String != "" {
		completedAt, err := time.Parse(time.RFC3339Nano, completedStr.String)
		if err != nil {
			return files.UploadSession{}, fmt.Errorf("parse session completed_at %q: %w", completedStr.String, err)
		}
		utc := completedAt.UTC()
		session.CompletedAt = &utc
	}
	return session, nil
}
