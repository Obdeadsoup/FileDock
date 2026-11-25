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
	if len(args) < 2 || args[0] != "sync" || args[1] != "pull" {
		return errors.New("usage: filedock sync pull --server <URL> --dir <local-dir>")
	}

	fs := flag.NewFlagSet("sync pull", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	serverURL := fs.String("server", "", "FileDock server base URL")
	dir := fs.String("dir", "", "local mirror directory")
	if err := fs.Parse(args[2:]); err != nil {
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
		ServerURL: *serverURL,
		Token:     token,
		Dir:       *dir,
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
