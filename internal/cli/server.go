package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/iluxav/sqlite-admin/sqliteadmin"
	_ "modernc.org/sqlite"
)

var errNotRunning = errors.New("not running")

type instance struct {
	PID       int       `json:"pid"`
	Database  string    `json:"database"`
	Address   string    `json:"address"`
	ReadOnly  bool      `json:"read_only"`
	StartedAt time.Time `json:"started_at"`
	LogFile   string    `json:"log_file,omitempty"`
}

func serve(o options, stop <-chan struct{}, ready func(instance) error) (returnErr error) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Bind first: an occupied port must not start a backup scheduler.
	listener, err := net.Listen("tcp", o.Address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.Address, err)
	}
	defer listener.Close()
	admin, err := sqliteadmin.New(sqliteadmin.Config{Path: o.Database, Addr: o.Address, ReadOnly: o.ReadOnly, BackupDir: o.BackupDir})
	if err != nil {
		return err
	}
	defer func() {
		shutdown, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := admin.Shutdown(shutdown); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("shutdown: %w", err))
		}
	}()
	result := make(chan error, 1)
	go func() { result <- admin.Serve(listener) }()
	state := instance{PID: os.Getpid(), Database: o.Database, Address: listener.Addr().String(), ReadOnly: o.ReadOnly, StartedAt: time.Now().UTC(), LogFile: o.LogFile}
	if ready != nil {
		if err := ready(state); err != nil {
			return err
		}
	}
	select {
	case <-ctx.Done():
	case <-stop:
	case err := <-result:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	slog.Info("shutting down")
	return nil
}
