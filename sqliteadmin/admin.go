// Package sqliteadmin embeds a password-protected web UI for browsing and
// editing a SQLite database into any Go binary.
//
// The UI (HTML templates, htmx and CSS) is compiled into the binary and served
// from its own http.Server, separate from the host application's listener, so
// the developer decides how (and whether) to expose it:
//
//	import _ "modernc.org/sqlite" // or mattn/go-sqlite3, ncruces/go-sqlite3, ...
//
//	admin, err := sqliteadmin.New(sqliteadmin.Config{Path: "data/app.db"})
//	if err != nil { log.Fatal(err) }
//	go admin.ListenAndServe()
//	defer admin.Shutdown(context.Background())
//
// The library does not import a SQLite driver itself; it uses the one the host
// application already registered with database/sql.
package sqliteadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Environment variables read when Config.Username / Config.Password are empty.
const (
	EnvUsername = "SQLITEADMIN_USER"
	EnvPassword = "SQLITEADMIN_PASSWORD"
	// EnvBehindProxy turns Config.BehindProxy on: "1", "true", "yes" or "on".
	EnvBehindProxy = "SQLITEADMIN_BEHIND_PROXY"
)

// Config configures an Admin.
type Config struct {
	// Path to an existing SQLite database file. Required. The file is never
	// created: a missing file is an error.
	Path string

	// Addr is the listen address of the admin UI. Default "127.0.0.1:8081"
	// (loopback only).
	Addr string

	// Driver is the database/sql driver name to open Path with. When empty, the
	// SQLite driver registered by the host ("sqlite3" or "sqlite") is detected.
	Driver string

	// Username and Password protect the UI. When empty they are read from the
	// SQLITEADMIN_USER and SQLITEADMIN_PASSWORD environment variables. New fails
	// if either is still empty.
	Username string
	Password string

	// ReadOnly disables every write path: cell edits, row inserts/deletes,
	// schema changes, writing SQL, saving snippets, backup creation and restore.
	ReadOnly bool

	// BehindProxy says the UI is served through one reverse proxy that sets
	// X-Forwarded-For and X-Forwarded-Proto (Caddy, nginx): the login limiter
	// then keys on the last forwarded address, the one that proxy appended,
	// instead of the proxy's own, and the session cookie is marked Secure when
	// the forwarded scheme is https. Default false, or
	// SQLITEADMIN_BEHIND_PROXY=true. Leave it off when clients connect
	// directly: they could set those headers.
	BehindProxy bool

	// ClientAddress, when set, names the client for the login limiter: for a
	// host behind more than one proxy (a CDN in front of Caddy), which knows
	// which header to trust. It takes precedence over BehindProxy's address.
	ClientAddress func(*http.Request) string

	// SessionTimeout is how long an idle login session stays valid.
	// Default 1 hour.
	SessionTimeout time.Duration

	// Logger receives startup, audit (commits, schema changes) and error
	// logs. Default slog.Default().
	Logger *slog.Logger

	// BackupDir holds local snapshots and the scheduler state. Default:
	// SQLITEADMIN_BACKUP_DIR, or Path + ".backups". Keep it on persistent storage.
	BackupDir string
	// S3 is optional. Nil reads SQLITEADMIN_S3_* environment variables.
	S3 *S3Config
	// BackupTimeout bounds a backup or restore. Default 5 minutes.
	BackupTimeout time.Duration
	// MaxRestoreBytes limits uploaded and downloaded restore files. Default 1 GiB.
	MaxRestoreBytes int64
	// Optional online-copy adapters for drivers other than modernc or mattn.
	// The connection is exclusively owned for the duration of the call.
	BackupCopy  func(context.Context, *sql.Conn, string) error
	RestoreCopy func(context.Context, *sql.Conn, string) error
}

// Admin is an embedded SQLite admin UI bound to one database file.
type Admin struct {
	cfg        Config
	driver     string
	db         *sql.DB
	log        *slog.Logger
	tmpl       *templates
	sessions   *sessionStore
	limiter    *loginLimiter
	handler    http.Handler
	srv        *http.Server
	databaseMu sync.RWMutex // restore excludes all other admin requests
	backups    *backupManager
}

// New validates cfg, opens its own small connection pool to the database and
// prepares the UI and starts the backup scheduler. It does not start listening;
// call ListenAndServe. Call Shutdown even when serving Handler separately.
func New(cfg Config) (*Admin, error) {
	if cfg.Path == "" {
		return nil, errors.New("sqliteadmin: Config.Path is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8081"
	}
	if cfg.Username == "" {
		cfg.Username = os.Getenv(EnvUsername)
	}
	if cfg.Password == "" {
		cfg.Password = os.Getenv(EnvPassword)
	}
	if !cfg.BehindProxy {
		cfg.BehindProxy = envBool(EnvBehindProxy)
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, fmt.Errorf("sqliteadmin: username and password are required (set %s and %s, or Config.Username/Config.Password)", EnvUsername, EnvPassword)
	}
	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BackupTimeout <= 0 {
		cfg.BackupTimeout = 5 * time.Minute
	}
	if cfg.MaxRestoreBytes <= 0 {
		cfg.MaxRestoreBytes = 1 << 30
	}

	st, err := os.Stat(cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("sqliteadmin: database file: %w", err)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("sqliteadmin: database path %q is a directory", cfg.Path)
	}

	driver, err := detectDriver(cfg.Driver)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("sqliteadmin: open: %w", err)
	}
	// A deliberately small pool of our own, so admin queries never take
	// connections away from the host application.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)

	a := &Admin{
		cfg:      cfg,
		driver:   driver,
		db:       db,
		log:      cfg.Logger.With("component", "sqliteadmin"),
		sessions: newSessionStore(cfg.SessionTimeout),
		limiter:  newLoginLimiter(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.verify(ctx); err != nil {
		db.Close()
		return nil, err
	}

	a.tmpl, err = parseTemplates()
	if err != nil {
		db.Close()
		return nil, err
	}
	a.backups, err = newBackupManager(a)
	if err != nil {
		db.Close()
		return nil, err
	}
	a.handler = a.routes()
	a.srv = &http.Server{
		Addr:              cfg.Addr,
		Handler:           a.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.BackupTimeout + 30*time.Second,
		WriteTimeout:      cfg.BackupTimeout + 30*time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go a.backups.run()
	return a, nil
}

func (a *Admin) verify(ctx context.Context) error {
	c, err := a.conn(ctx)
	if err != nil {
		return fmt.Errorf("sqliteadmin: connect: %w", err)
	}
	defer c.Close()
	var n int
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		return fmt.Errorf("sqliteadmin: %s is not a readable SQLite database: %w", a.cfg.Path, err)
	}
	return nil
}

// ListenAndServe starts the admin UI on Config.Addr. It blocks until the
// server stops and, like http.Server, returns http.ErrServerClosed after
// Shutdown.
func (a *Admin) ListenAndServe() error {
	ln, err := net.Listen("tcp", a.cfg.Addr)
	if err != nil {
		return err
	}
	return a.Serve(ln)
}

// Serve starts the admin UI on an existing listener and takes ownership of it.
// Like http.Server.Serve, it blocks and returns http.ErrServerClosed after Shutdown.
func (a *Admin) Serve(ln net.Listener) error {
	addr := ln.Addr().String()
	if !isLoopback(addr) {
		a.log.Warn("admin UI listens on a non-loopback address; put it behind TLS", "addr", addr)
	}
	a.log.Info("admin UI listening", "url", "http://"+addr, "db", a.cfg.Path, "driver", a.driver, "read_only", a.cfg.ReadOnly)
	return a.srv.Serve(ln)
}

// Shutdown gracefully stops the server and closes the admin's database pool.
func (a *Admin) Shutdown(ctx context.Context) error {
	a.backups.cancel()
	err := a.srv.Shutdown(ctx)
	select {
	case <-a.backups.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	a.backups.cleanupUploads()
	if cerr := a.db.Close(); err == nil {
		err = cerr
	}
	return err
}

// Handler returns the fully authenticated UI handler, for serving it from an
// http.Server you configure yourself (e.g. with TLS). It must be mounted at
// the root path "/".
func (a *Admin) Handler() http.Handler { return a.handler }

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// envBool reads a yes/no setting from the environment; unset or anything else is false.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
