package files

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	ResourceTypeShare = "share"

	AuditActionFileUpload        = "file.upload"
	AuditActionFileContentUpdate = "file.content_update"
	AuditActionFileUpdate        = "file.update"
	AuditActionFileDelete        = "file.delete"
	AuditActionFolderCreate      = "folder.create"
	AuditActionFolderUpdate      = "folder.update"
	AuditActionFolderDelete      = "folder.delete"
	AuditActionShareCreate       = "share.create"
	AuditActionShareDelete       = "share.delete"

	DefaultAuditListLimit = 50
	MaxAuditListLimit     = 500
)

var ErrInvalidAuditFilter = errors.New("invalid audit filter")

// AuditEntry represents a single persisted operation audit record.
// It intentionally excludes passwords, tokens, file bodies, and disk object paths.
type AuditEntry struct {
	ID           int64
	ActorUserID  string
	Action       string
	ResourceType string
	ResourceID   string
	OccurredAt   time.Time
}

// AuditFilter controls filtering and bounded result size for audit log queries.
type AuditFilter struct {
	ActorUserID string
	Action      string
	Limit       int
}

// AuditStore queries persisted operation audit records.
type AuditStore interface {
	ListAuditLogs(ctx context.Context, filter AuditFilter) ([]AuditEntry, error)
}

// NormalizeAuditFilter validates and bounds an AuditFilter.
func NormalizeAuditFilter(f AuditFilter) (AuditFilter, error) {
	f.ActorUserID = strings.TrimSpace(f.ActorUserID)
	f.Action = strings.TrimSpace(f.Action)
	if f.Limit < 0 {
		return AuditFilter{}, fmt.Errorf("%w: limit must be non-negative", ErrInvalidAuditFilter)
	}
	if f.Limit == 0 {
		f.Limit = DefaultAuditListLimit
	}
	if f.Limit > MaxAuditListLimit {
		return AuditFilter{}, fmt.Errorf("%w: limit exceeds maximum %d", ErrInvalidAuditFilter, MaxAuditListLimit)
	}
	return f, nil
}
