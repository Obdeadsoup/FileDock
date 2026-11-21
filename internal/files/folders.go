package files

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrFolderNotFound  = errors.New("folder not found")
	ErrFolderNameTaken = errors.New("folder name already exists in this directory")
	ErrFolderNotEmpty  = errors.New("folder is not empty")
	ErrFolderCycle     = errors.New("cannot move a folder into itself or its descendants")
	ErrEmptyChanges    = errors.New("name or parent_id is required")
)

type Folder struct {
	ID        string
	Name      string
	ParentID  *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Changes distinguishes leaving a parent unchanged from moving to the root.
type Changes struct {
	Name      *string
	SetParent bool
	ParentID  *string
}

// TreeStore performs metadata changes with ownership and tree checks in the same transaction.
type TreeStore interface {
	CheckFolder(ctx context.Context, ownerID, id string) error
	CreateFolder(ctx context.Context, ownerID string, folder Folder) error
	ListFolders(ctx context.Context, ownerID string, page ListPage) ([]Folder, error)
	UpdateFolder(ctx context.Context, ownerID, id string, changes Changes, now time.Time) (Folder, error)
	DeleteFolder(ctx context.Context, ownerID, id string) error
	UpdateFile(ctx context.Context, ownerID, id string, changes Changes) (File, error)
}

func validateLocation(ownerID string, parentID *string) error {
	if err := ValidateID(ownerID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidOwner, err)
	}
	if parentID != nil {
		return ValidateID(*parentID)
	}
	return nil
}

func newPage(parentID *string, limit, offset int) (ListPage, error) {
	if limit < 0 || offset < 0 {
		return ListPage{}, ErrInvalidPagination
	}
	if limit == 0 {
		limit = DefaultListLimit
	}
	return ListPage{Limit: min(limit, MaxListLimit), Offset: offset, ParentID: parentID}, nil
}

func (c Changes) validate(ownerID, id string) error {
	if err := validateLocation(ownerID, c.ParentID); err != nil {
		return err
	}
	if err := ValidateID(id); err != nil {
		return err
	}
	if c.Name != nil {
		return ValidateFilename(*c.Name)
	}
	if !c.SetParent {
		return ErrEmptyChanges
	}
	return nil
}

func (s *Service) CheckParent(ctx context.Context, ownerID string, parentID *string) error {
	if err := validateLocation(ownerID, parentID); err != nil {
		return err
	}
	if parentID == nil {
		return nil
	}
	return s.meta.CheckFolder(ctx, ownerID, *parentID)
}

func (s *Service) CreateFolder(ctx context.Context, ownerID string, parentID *string, name string) (Folder, error) {
	if err := validateLocation(ownerID, parentID); err != nil {
		return Folder{}, err
	}
	if err := ValidateFilename(name); err != nil {
		return Folder{}, err
	}
	id, err := s.idGen()
	if err != nil {
		return Folder{}, fmt.Errorf("generate folder id: %w", err)
	}
	if err := ValidateID(id); err != nil {
		return Folder{}, fmt.Errorf("generated invalid folder id: %w", err)
	}
	now := s.now().UTC()
	folder := Folder{ID: id, Name: name, ParentID: parentID, CreatedAt: now, UpdatedAt: now}
	if err := s.meta.CreateFolder(ctx, ownerID, folder); err != nil {
		return Folder{}, err
	}
	return folder, nil
}

func (s *Service) ListFolders(ctx context.Context, ownerID string, parentID *string, limit, offset int) ([]Folder, error) {
	if err := validateLocation(ownerID, parentID); err != nil {
		return nil, err
	}
	page, err := newPage(parentID, limit, offset)
	if err != nil {
		return nil, err
	}
	return s.meta.ListFolders(ctx, ownerID, page)
}

func (s *Service) UpdateFolder(ctx context.Context, ownerID, id string, changes Changes) (Folder, error) {
	if err := changes.validate(ownerID, id); err != nil {
		return Folder{}, err
	}
	return s.meta.UpdateFolder(ctx, ownerID, id, changes, s.now().UTC())
}

func (s *Service) DeleteFolder(ctx context.Context, ownerID, id string) error {
	if err := validateLocation(ownerID, &id); err != nil {
		return err
	}
	return s.meta.DeleteFolder(ctx, ownerID, id)
}
