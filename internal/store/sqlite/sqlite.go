package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"example.com/filedock/internal/files"
)

const schemaDDL = `
CREATE TABLE IF NOT EXISTS files (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	object_key TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_files_created_at_ns_id
	ON files (created_at_ns DESC, id ASC);
`

// Store persists file metadata in SQLite via database/sql.
type Store struct {
	db *sql.DB
}

// NewStore opens or creates the SQLite database under dataDir and applies idempotent schema DDL.
func NewStore(ctx context.Context, dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("data dir must not be empty")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dbPath := filepath.Join(dataDir, "filedock.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := initDB(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func initDB(ctx context.Context, db *sql.DB) error {
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000; PRAGMA journal_mode = WAL;`); err != nil {
		return fmt.Errorf("configure sqlite pragmas: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaDDL); err != nil {
		return fmt.Errorf("initialize sqlite schema: %w", err)
	}
	return nil
}

// Close closes the underlying database connection pool.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Create inserts a new file metadata record.
func (s *Store) Create(ctx context.Context, file files.StoredFile) error {
	createdUTC := file.CreatedAt.UTC()
	const query = `
INSERT INTO files (id, name, size, sha256, object_key, created_at, created_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(
		ctx,
		query,
		file.ID,
		file.Name,
		file.Size,
		file.SHA256,
		file.ObjectKey,
		createdUTC.Format(time.RFC3339Nano),
		createdUTC.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("insert file metadata: %w", err)
	}
	return nil
}

// List returns registered files ordered stably by creation time descending, then ID ascending.
func (s *Store) List(ctx context.Context, page files.ListPage) ([]files.File, error) {
	const query = `
SELECT id, name, size, sha256, created_at
FROM files
ORDER BY created_at_ns DESC, id ASC
LIMIT ? OFFSET ?`

	rows, err := s.db.QueryContext(ctx, query, page.Limit, page.Offset)
	if err != nil {
		return nil, fmt.Errorf("query files: %w", err)
	}
	defer rows.Close()

	result := make([]files.File, 0)
	for rows.Next() {
		var (
			item         files.File
			createdAtStr string
		)
		if err := rows.Scan(&item.ID, &item.Name, &item.Size, &item.SHA256, &createdAtStr); err != nil {
			return nil, fmt.Errorf("scan file row: %w", err)
		}
		parsedAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("parse created_at %q: %w", createdAtStr, err)
		}
		item.CreatedAt = parsedAt.UTC()
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate file rows: %w", err)
	}
	return result, nil
}

// GetByID retrieves stored file metadata and its object key by ID.
func (s *Store) GetByID(ctx context.Context, id string) (files.StoredFile, error) {
	const query = `
SELECT id, name, size, sha256, object_key, created_at
FROM files
WHERE id = ?`

	var (
		record       files.StoredFile
		createdAtStr string
	)
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&record.ID,
		&record.Name,
		&record.Size,
		&record.SHA256,
		&record.ObjectKey,
		&createdAtStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return files.StoredFile{}, files.ErrNotFound
		}
		return files.StoredFile{}, fmt.Errorf("get file by id: %w", err)
	}
	parsedAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return files.StoredFile{}, fmt.Errorf("parse created_at %q: %w", createdAtStr, err)
	}
	record.CreatedAt = parsedAt.UTC()
	return record, nil
}
