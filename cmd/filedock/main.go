package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"example.com/filedock/internal/syncclient"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "filedock: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: filedock <sync pull|sync push|upload> [flags]")
	}

	switch args[0] {
	case "sync":
		if len(args) < 2 {
			return errors.New("usage: filedock sync <pull|push> --server <URL> --dir <local-dir> [--path <rel-path>]")
		}
		switch args[1] {
		case "pull":
			return runSyncPull(ctx, args[2:], stdout)
		case "push":
			return runSyncPush(ctx, args[2:], stdout)
		default:
			return fmt.Errorf("unknown sync subcommand %q (expected pull or push)", args[1])
		}
	case "upload":
		return runUpload(ctx, args[1:], stdout)
	default:
		return fmt.Errorf("unknown command %q (expected sync or upload)", args[0])
	}
}

func resolveCACert(flagVal string) string {
	if trimmed := strings.TrimSpace(flagVal); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(os.Getenv("FILEDOCK_CA_CERT"))
}

func runSyncPull(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sync pull", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	serverURL := fs.String("server", "", "FileDock server base URL")
	dir := fs.String("dir", "", "local mirror directory")
	caCert := fs.String("ca-cert", "", "optional PEM CA certificate file for HTTPS verification")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	token := strings.TrimSpace(os.Getenv("FILEDOCK_TOKEN"))
	if token == "" {
		return errors.New("FILEDOCK_TOKEN environment variable is required")
	}

	res, err := syncclient.Pull(ctx, syncclient.Config{
		ServerURL:  *serverURL,
		Token:      token,
		Dir:        *dir,
		CACertFile: resolveCACert(*caCert),
	})
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(
		stdout,
		"synced user=%s cursor=%d->%d events=%d downloaded=%d reused=%d deleted=%d\n",
		res.UserID,
		res.StartCursor,
		res.EndCursor,
		res.EventsApplied,
		res.Downloaded,
		res.Reused,
		res.Deleted,
	)
	return nil
}

func runSyncPush(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sync push", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	serverURL := fs.String("server", "", "FileDock server base URL")
	dir := fs.String("dir", "", "local mirror directory")
	relPath := fs.String("path", "", "relative path of tracked file inside mirror directory")
	caCert := fs.String("ca-cert", "", "optional PEM CA certificate file for HTTPS verification")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	token := strings.TrimSpace(os.Getenv("FILEDOCK_TOKEN"))
	if token == "" {
		return errors.New("FILEDOCK_TOKEN environment variable is required")
	}

	res, err := syncclient.Push(ctx, syncclient.PushConfig{
		ServerURL:  *serverURL,
		Token:      token,
		Dir:        *dir,
		RelPath:    *relPath,
		CACertFile: resolveCACert(*caCert),
	})
	if err != nil {
		return err
	}

	status := "pushed"
	if res.Skipped {
		status = "unchanged"
	} else if res.Recovered {
		status = "recovered"
	}
	_, _ = fmt.Fprintf(
		stdout,
		"%s file=%s path=%s revision=%d->%d sha256=%s cursor=%d\n",
		status,
		res.FileID,
		res.LocalPath,
		res.OldRevision,
		res.NewRevision,
		res.SHA256,
		res.Cursor,
	)
	return nil
}

func runUpload(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	serverURL := fs.String("server", "", "FileDock server base URL")
	filePath := fs.String("file", "", "local file to upload")
	parentID := fs.String("parent-id", "", "optional target folder ID on server")
	sessionID := fs.String("session", "", "optional upload session ID to resume")
	caCert := fs.String("ca-cert", "", "optional PEM CA certificate file for HTTPS verification")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	token := strings.TrimSpace(os.Getenv("FILEDOCK_TOKEN"))
	if token == "" {
		return errors.New("FILEDOCK_TOKEN environment variable is required")
	}

	var parentPtr *string
	if trimmedParent := strings.TrimSpace(*parentID); trimmedParent != "" {
		parentPtr = &trimmedParent
	}

	res, err := syncclient.Upload(ctx, syncclient.UploadConfig{
		ServerURL:  *serverURL,
		Token:      token,
		FilePath:   *filePath,
		ParentID:   parentPtr,
		SessionID:  *sessionID,
		CACertFile: resolveCACert(*caCert),
		OnSessionReady: func(session syncclient.UploadSessionInfo) {
			_, _ = fmt.Fprintf(
				stdout,
				"session=%s file=%s size=%d chunks=%d chunk_size=%d confirmed=%d\n",
				session.ID,
				session.Name,
				session.Size,
				session.TotalChunks,
				session.ChunkSize,
				len(session.UploadedChunks),
			)
		},
	})
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(
		stdout,
		"uploaded file=%s session=%s size=%d bytes_sent=%d chunks_sent=%d/%d sha256=%s\n",
		res.File.ID,
		res.SessionID,
		res.File.Size,
		res.BytesSent,
		res.ChunksSent,
		res.TotalChunks,
		res.File.SHA256,
	)
	return nil
}
