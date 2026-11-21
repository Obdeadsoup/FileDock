package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/files"
)

const baseSchemaDDL = `
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	username TEXT NOT NULL UNIQUE,
	password_hash BLOB NOT NULL,
	password_salt BLOB NOT NULL,
	argon_time INTEGER NOT NULL,
	argon_memory INTEGER NOT NULL,
	argon_threads INTEGER NOT NULL,
	argon_key_len INTEGER NOT NULL,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	expires_at_ns INTEGER NOT NULL,
	FOREIGN KEY (user_id) REFERENCES users(id)
);

CREATE INDEX IF NOT EXISTS idx_sessions_user_id
	ON sessions (user_id);

CREATE TABLE IF NOT EXISTS files (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	size INTEGER NOT NULL,
	sha256 TEXT NOT NULL,
	object_key TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL,
	created_at_ns INTEGER NOT NULL,
	owner_id TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_files_created_at_ns_id
	ON files (created_at_ns DESC, id ASC);
`

// Store persists user accounts, session digests, and file metadata in SQLite via database/sql.
type Store struct {
	db *sql.DB
}

// NewStore opens or creates the SQLite database under dataDir and applies transactional, idempotent schema migrations.
func NewStore(ctx context.Context, dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("data dir must not be empty")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	dbPath, err := filepath.Abs(filepath.Join(dataDir, "filedock.db"))
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite path: %w", err)
	}
	path := filepath.ToSlash(dbPath)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"}
	db, err := sql.Open("sqlite", dsn.String())
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
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("configure sqlite pragmas: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, baseSchemaDDL); err != nil {
		return fmt.Errorf("initialize base sqlite schema: %w", err)
	}

	hasOwnerID, err := tableHasColumn(ctx, tx, "files", "owner_id")
	if err != nil {
		return err
	}
	if !hasOwnerID {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE files ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add owner_id column to files: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE files SET owner_id = '' WHERE owner_id IS NULL`); err != nil {
		return fmt.Errorf("normalize legacy file owner_id: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_files_owner_created_at_ns_id
	ON files (owner_id, created_at_ns DESC, id ASC)`); err != nil {
		return fmt.Errorf("create files owner index: %w", err)
	}
	if err := migrateFolders(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration transaction: %w", err)
	}
	return nil
}

func tableHasColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("query table_info(%s): %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return false, fmt.Errorf("scan table_info(%s): %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate table_info(%s): %w", table, err)
	}
	return false, nil
}

// Close closes the underlying database connection pool.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// CreateUser inserts a new user account and its Argon2id verification parameters.
func (s *Store) CreateUser(ctx context.Context, user accounts.StoredUser) error {
	createdUTC := user.CreatedAt.UTC()
	const query = `
INSERT INTO users (
	id, username, password_hash, password_salt,
	argon_time, argon_memory, argon_threads, argon_key_len,
	created_at, created_at_ns
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(
		ctx,
		query,
		user.ID,
		user.Username,
		user.PasswordHash,
		user.PasswordSalt,
		user.ArgonParams.Time,
		user.ArgonParams.Memory,
		user.ArgonParams.Threads,
		user.ArgonParams.KeyLen,
		createdUTC.Format(time.RFC3339Nano),
		createdUTC.UnixNano(),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return accounts.ErrUsernameTaken
		}
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

// GetUserByUsername fetches a stored user record by username.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (accounts.StoredUser, error) {
	const query = `
SELECT id, username, password_hash, password_salt,
       argon_time, argon_memory, argon_threads, argon_key_len, created_at
FROM users
WHERE username = ?`
	return s.scanUser(s.db.QueryRowContext(ctx, query, username))
}

// GetUserByID fetches a stored user record by user ID.
func (s *Store) GetUserByID(ctx context.Context, id string) (accounts.StoredUser, error) {
	const query = `
SELECT id, username, password_hash, password_salt,
       argon_time, argon_memory, argon_threads, argon_key_len, created_at
FROM users
WHERE id = ?`
	return s.scanUser(s.db.QueryRowContext(ctx, query, id))
}

func (s *Store) scanUser(row *sql.Row) (accounts.StoredUser, error) {
	var (
		user         accounts.StoredUser
		threads      uint32
		createdAtStr string
	)
	err := row.Scan(
		&user.ID,
		&user.Username,
		&user.PasswordHash,
		&user.PasswordSalt,
		&user.ArgonParams.Time,
		&user.ArgonParams.Memory,
		&threads,
		&user.ArgonParams.KeyLen,
		&createdAtStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return accounts.StoredUser{}, accounts.ErrUserNotFound
		}
		return accounts.StoredUser{}, fmt.Errorf("scan user: %w", err)
	}
	user.ArgonParams.Threads = uint8(threads)
	parsedAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return accounts.StoredUser{}, fmt.Errorf("parse user created_at %q: %w", createdAtStr, err)
	}
	user.CreatedAt = parsedAt.UTC()
	return user, nil
}

// CreateSession stores a session token digest and its expiration time.
func (s *Store) CreateSession(ctx context.Context, session accounts.StoredSession) error {
	createdUTC := session.CreatedAt.UTC()
	expiresUTC := session.ExpiresAt.UTC()
	const query = `
INSERT INTO sessions (token_hash, user_id, created_at, expires_at, expires_at_ns)
VALUES (?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(
		ctx,
		query,
		session.TokenHash,
		session.UserID,
		createdUTC.Format(time.RFC3339Nano),
		expiresUTC.Format(time.RFC3339Nano),
		expiresUTC.UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetSessionByTokenHash looks up a session record by its SHA-256 token digest.
func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash string) (accounts.StoredSession, error) {
	const query = `
SELECT token_hash, user_id, created_at, expires_at
FROM sessions
WHERE token_hash = ?`
	var (
		session      accounts.StoredSession
		createdAtStr string
		expiresAtStr string
	)
	err := s.db.QueryRowContext(ctx, query, tokenHash).Scan(
		&session.TokenHash,
		&session.UserID,
		&createdAtStr,
		&expiresAtStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return accounts.StoredSession{}, accounts.ErrUnauthorized
		}
		return accounts.StoredSession{}, fmt.Errorf("get session: %w", err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return accounts.StoredSession{}, fmt.Errorf("parse session created_at %q: %w", createdAtStr, err)
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresAtStr)
	if err != nil {
		return accounts.StoredSession{}, fmt.Errorf("parse session expires_at %q: %w", expiresAtStr, err)
	}
	session.CreatedAt = createdAt.UTC()
	session.ExpiresAt = expiresAt.UTC()
	return session, nil
}

// DeleteSessionByTokenHash revokes a session by its token digest.
func (s *Store) DeleteSessionByTokenHash(ctx context.Context, tokenHash string) error {
	const query = `DELETE FROM sessions WHERE token_hash = ?`
	res, err := s.db.ExecContext(ctx, query, tokenHash)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("session delete rows affected: %w", err)
	}
	if affected == 0 {
		return accounts.ErrUnauthorized
	}
	return nil
}

// Create inserts a new file metadata record scoped to file.OwnerID.
func (s *Store) Create(ctx context.Context, file files.StoredFile) error {
	if strings.TrimSpace(file.OwnerID) == "" {
		return files.ErrInvalidOwner
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin file registration: %w", err)
	}
	defer tx.Rollback()
	if err := checkParent(ctx, tx, file.OwnerID, file.ParentID); err != nil {
		return err
	}
	createdUTC := file.CreatedAt.UTC()
	const query = `
INSERT INTO files (id, owner_id, parent_id, name, size, sha256, object_key, created_at, created_at_ns)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = tx.ExecContext(
		ctx,
		query,
		file.ID,
		file.OwnerID,
		file.ParentID,
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
	return tx.Commit()
}

// List returns registered files owned by ownerID ordered stably by creation time descending, then ID ascending.
func (s *Store) List(ctx context.Context, ownerID string, page files.ListPage) ([]files.File, error) {
	if strings.TrimSpace(ownerID) == "" {
		return nil, files.ErrInvalidOwner
	}
	const query = `
SELECT id, name, parent_id, size, sha256, created_at
FROM files
WHERE owner_id = ? AND parent_id IS ?
ORDER BY created_at_ns DESC, id ASC
LIMIT ? OFFSET ?`

	tx, err := s.beginList(ctx, ownerID, page.ParentID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query, ownerID, page.ParentID, page.Limit, page.Offset)
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
		if err := rows.Scan(&item.ID, &item.Name, &item.ParentID, &item.Size, &item.SHA256, &createdAtStr); err != nil {
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

// GetByID retrieves stored file metadata and its object key only if both ID and ownerID match.
func (s *Store) GetByID(ctx context.Context, ownerID, id string) (files.StoredFile, error) {
	return getFile(ctx, s.db, ownerID, id)
}

func getFile(ctx context.Context, db rowQuerier, ownerID, id string) (files.StoredFile, error) {
	if strings.TrimSpace(ownerID) == "" {
		return files.StoredFile{}, files.ErrNotFound
	}
	const query = `
SELECT id, owner_id, name, parent_id, size, sha256, object_key, created_at
FROM files
WHERE id = ? AND owner_id = ?`

	var (
		record       files.StoredFile
		createdAtStr string
	)
	err := db.QueryRowContext(ctx, query, id, ownerID).Scan(
		&record.ID,
		&record.OwnerID,
		&record.Name,
		&record.ParentID,
		&record.Size,
		&record.SHA256,
		&record.ObjectKey,
		&createdAtStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return files.StoredFile{}, files.ErrNotFound
		}
		return files.StoredFile{}, fmt.Errorf("get file by id and owner: %w", err)
	}
	parsedAt, err := time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return files.StoredFile{}, fmt.Errorf("parse created_at %q: %w", createdAtStr, err)
	}
	record.CreatedAt = parsedAt.UTC()
	return record, nil
}

// ClaimLegacyFiles assigns all unclaimed legacy files (owner_id = ”) to the existing user identified by username.
func (s *Store) ClaimLegacyFiles(ctx context.Context, username string) (int64, error) {
	if strings.TrimSpace(username) == "" {
		return 0, accounts.ErrInvalidUsername
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin claim-legacy transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var userID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, accounts.ErrUserNotFound
		}
		return 0, fmt.Errorf("find owner %q: %w", username, err)
	}

	res, err := tx.ExecContext(ctx, `UPDATE files SET owner_id = ?, parent_id = NULL WHERE owner_id = '' OR owner_id IS NULL`, userID)
	if err != nil {
		return 0, fmt.Errorf("claim legacy files: %w", err)
	}
	claimed, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("claim legacy rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit claim-legacy transaction: %w", err)
	}
	return claimed, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed")
}
