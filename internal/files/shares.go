package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	ResourceTypeFile       = "file"
	ResourceTypeFolder     = "folder"
	ShareDirectionSent     = "sent"
	ShareDirectionReceived = "received"
	SharePermissionRead    = "read"
)

var (
	ErrInvalidResourceType   = errors.New("resource_type must be file or folder")
	ErrInvalidShareDirection = errors.New("direction must be sent or received")
	ErrInvalidRecipient      = errors.New("invalid recipient_username")
	ErrSelfShare             = errors.New("cannot share a resource with yourself")
	ErrRecipientNotFound     = errors.New("recipient user not found")
	ErrShareNotFound         = errors.New("share not found")
)

// Share describes a read-only share authorization between an owner and a recipient account.
type Share struct {
	ID                string
	ResourceType      string
	ResourceID        string
	ResourceName      string
	OwnerID           string
	OwnerUsername     string
	RecipientID       string
	RecipientUsername string
	Permission        string
	CreatedAt         time.Time
}

// CreateShareInput carries the parameters for creating a read-only share.
type CreateShareInput struct {
	ResourceType      string
	ResourceID        string
	RecipientUsername string
}

// ShareStore persists and queries share authorizations and share-scoped read views.
type ShareStore interface {
	CreateShare(ctx context.Context, ownerID, shareID string, input CreateShareInput, now time.Time) (Share, bool, error)
	ListShares(ctx context.Context, userID, direction string, limit, offset int) ([]Share, error)
	DeleteShare(ctx context.Context, ownerID, shareID string) error
	ListSharedFolders(ctx context.Context, recipientID, shareID string, page ListPage) ([]Folder, error)
	ListSharedFiles(ctx context.Context, recipientID, shareID string, page ListPage) ([]File, error)
	GetSharedFile(ctx context.Context, recipientID, shareID, fileID string) (StoredFile, error)
}

// CreateShare grants read-only access to a file or folder to recipient_username.
// If the same share already exists, the existing Share is returned with created=false.
func (s *Service) CreateShare(ctx context.Context, ownerID string, input CreateShareInput) (Share, bool, error) {
	if err := ValidateID(ownerID); err != nil {
		return Share{}, false, fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if input.ResourceType != ResourceTypeFile && input.ResourceType != ResourceTypeFolder {
		return Share{}, false, ErrInvalidResourceType
	}
	if err := ValidateID(input.ResourceID); err != nil {
		return Share{}, false, err
	}
	if err := validateRecipientUsername(input.RecipientUsername); err != nil {
		return Share{}, false, err
	}

	shareID, err := s.idGen()
	if err != nil {
		return Share{}, false, fmt.Errorf("generate share id: %w", err)
	}
	if err := ValidateID(shareID); err != nil {
		return Share{}, false, fmt.Errorf("generated invalid share id: %w", err)
	}

	return s.meta.CreateShare(ctx, ownerID, shareID, input, s.now().UTC())
}

// ListShares returns a bounded page of shares sent or received by userID.
func (s *Service) ListShares(ctx context.Context, userID, direction string, limit, offset int) ([]Share, error) {
	if err := ValidateID(userID); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if direction != ShareDirectionSent && direction != ShareDirectionReceived {
		return nil, ErrInvalidShareDirection
	}
	page, err := newPage(nil, limit, offset)
	if err != nil {
		return nil, err
	}
	items, err := s.meta.ListShares(ctx, userID, direction, page.Limit, page.Offset)
	if err != nil {
		return nil, err
	}
	if items == nil {
		return []Share{}, nil
	}
	return items, nil
}

// DeleteShare revokes a share owned by ownerID.
func (s *Service) DeleteShare(ctx context.Context, ownerID, shareID string) error {
	if err := ValidateID(ownerID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if err := ValidateID(shareID); err != nil {
		return err
	}
	return s.meta.DeleteShare(ctx, ownerID, shareID)
}

// ListSharedFolders lists direct subfolders inside a shared folder subtree for recipientID.
func (s *Service) ListSharedFolders(ctx context.Context, recipientID, shareID string, parentID *string, limit, offset int) ([]Folder, error) {
	if err := validateLocation(recipientID, parentID); err != nil {
		return nil, err
	}
	if err := ValidateID(shareID); err != nil {
		return nil, err
	}
	page, err := newPage(parentID, limit, offset)
	if err != nil {
		return nil, err
	}
	items, err := s.meta.ListSharedFolders(ctx, recipientID, shareID, page)
	if err != nil {
		return nil, err
	}
	if items == nil {
		return []Folder{}, nil
	}
	return items, nil
}

// ListSharedFiles lists direct files inside a shared folder subtree (or the single shared file) for recipientID.
func (s *Service) ListSharedFiles(ctx context.Context, recipientID, shareID string, parentID *string, limit, offset int) ([]File, error) {
	if err := validateLocation(recipientID, parentID); err != nil {
		return nil, err
	}
	if err := ValidateID(shareID); err != nil {
		return nil, err
	}
	page, err := newPage(parentID, limit, offset)
	if err != nil {
		return nil, err
	}
	items, err := s.meta.ListSharedFiles(ctx, recipientID, shareID, page)
	if err != nil {
		return nil, err
	}
	if items == nil {
		return []File{}, nil
	}
	return items, nil
}

// OpenSharedContent verifies share authorization and subtree membership first, then opens the disk stream.
func (s *Service) OpenSharedContent(ctx context.Context, recipientID, shareID, fileID string) (File, io.ReadCloser, error) {
	if err := ValidateID(recipientID); err != nil {
		return File{}, nil, fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if err := ValidateID(shareID); err != nil {
		return File{}, nil, err
	}
	if err := ValidateID(fileID); err != nil {
		return File{}, nil, err
	}

	record, err := s.meta.GetSharedFile(ctx, recipientID, shareID, fileID)
	if err != nil {
		return File{}, nil, err
	}

	rc, err := s.blobs.Open(ctx, record.ObjectKey)
	if err != nil {
		return File{}, nil, err
	}
	return record.File, rc, nil
}

func validateRecipientUsername(username string) error {
	if strings.TrimSpace(username) == "" || len(username) > 64 {
		return ErrInvalidRecipient
	}
	for i := 0; i < len(username); i++ {
		b := username[i]
		if b <= 0x20 || b >= 0x7f {
			return ErrInvalidRecipient
		}
	}
	return nil
}
