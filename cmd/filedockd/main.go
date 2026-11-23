package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"example.com/filedock/internal/accounts"
	"example.com/filedock/internal/config"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/httpapi"
	"example.com/filedock/internal/store/localdisk"
	"example.com/filedock/internal/store/sqlite"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(os.Args[1:], os.Stdout, logger); err != nil {
		logger.Error("filedockd failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer, logger *slog.Logger) error {
	if len(args) > 0 {
		switch args[0] {
		case "claim-legacy":
			return runClaimLegacy(args[1:], stdout, logger)
		default:
			return fmt.Errorf("unknown command %q", args[0])
		}
	}
	return runServer(logger)
}

func runClaimLegacy(args []string, stdout io.Writer, logger *slog.Logger) error {
	fs := flag.NewFlagSet("claim-legacy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	owner := fs.String("owner", "", "existing username to claim unowned legacy files")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse claim-legacy flags: %w", err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments for claim-legacy: %v", fs.Args())
	}
	username := strings.TrimSpace(*owner)
	if username == "" {
		return errors.New("claim-legacy requires --owner <username>")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	metaStore, err := sqlite.NewStore(ctx, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("init sqlite store: %w", err)
	}
	defer func() {
		if closeErr := metaStore.Close(); closeErr != nil {
			logger.Error("close sqlite store failed", "error", closeErr)
		}
	}()

	claimed, err := metaStore.ClaimLegacyFiles(ctx, username)
	if err != nil {
		return fmt.Errorf("claim legacy files for %q: %w", username, err)
	}

	logger.Info("claimed legacy files", "owner", username, "claimed", claimed, "data_dir", cfg.DataDir)
	_, _ = fmt.Fprintf(stdout, "claimed %d legacy file(s) for owner %s\n", claimed, username)
	return nil
}

func runServer(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	blobStore, err := localdisk.NewStore(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("init localdisk store: %w", err)
	}

	initCtx, cancelInit := context.WithTimeout(context.Background(), 10*time.Second)
	metaStore, err := sqlite.NewStore(initCtx, cfg.DataDir)
	cancelInit()
	if err != nil {
		return fmt.Errorf("init sqlite store: %w", err)
	}
	defer func() {
		if closeErr := metaStore.Close(); closeErr != nil {
			logger.Error("close sqlite store failed", "error", closeErr)
		}
	}()

	accountService, err := accounts.NewService(metaStore, accounts.WithIDGenerator(newUUIDv4))
	if err != nil {
		return fmt.Errorf("init account service: %w", err)
	}

	fileService, err := files.NewService(blobStore, metaStore, newUUIDv4)
	if err != nil {
		return fmt.Errorf("init file service: %w", err)
	}

	cleanupCtx, cancelCleanup := context.WithCancel(context.Background())
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		fileService.RunCleanupWorker(
			cleanupCtx,
			files.DefaultCleanupInterval,
			files.DefaultCleanupBatchSize,
			func(cleanupErr error) {
				logger.Error("blob cleanup failed", "error", cleanupErr)
			},
		)
	}()
	defer func() {
		cancelCleanup()
		<-cleanupDone
	}()

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", cfg.Addr, err)
	}
	server := &http.Server{
		Handler:           httpapi.NewHandler(fileService, accountService),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	logger.Info("HTTP server listening", "addr", listener.Addr().String(), "data_dir", cfg.DataDir)

	select {
	case err = <-serveErr:
	case <-ctx.Done():
		stop()
		logger.Info("shutting down HTTP server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
			_ = server.Close()
			return fmt.Errorf("shut down HTTP server: %w", shutdownErr)
		}
		err = <-serveErr
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve HTTP: %w", err)
	}
	return nil
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
