package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/filedock/internal/config"
	"example.com/filedock/internal/files"
	"example.com/filedock/internal/httpapi"
	"example.com/filedock/internal/store/localdisk"
	"example.com/filedock/internal/store/sqlite"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("filedockd failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
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

	fileService, err := files.NewService(blobStore, metaStore, newUUIDv4)
	if err != nil {
		return fmt.Errorf("init file service: %w", err)
	}

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", cfg.Addr, err)
	}
	server := &http.Server{
		Handler:           httpapi.NewHandler(fileService),
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
