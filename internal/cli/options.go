package cli

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/joho/godotenv"
)

type options struct {
	Database, Address, EnvFile, BackupDir, StateDir, LogFile string
	ReadOnly                                                 bool
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var o options
	f := flag.NewFlagSet("sqliteadmin", flag.ContinueOnError)
	f.SetOutput(output)
	f.StringVar(&o.Database, "db", "", "existing SQLite database (or SQLITEADMIN_DB)")
	f.StringVar(&o.Address, "addr", "", "listen address (or SQLITEADMIN_ADDR; default 127.0.0.1:8081)")
	f.StringVar(&o.EnvFile, "env-file", ".env", "dotenv file; use --env-file= to disable loading")
	f.BoolVar(&o.ReadOnly, "read-only", false, "disable writes (or SQLITEADMIN_READ_ONLY)")
	f.StringVar(&o.BackupDir, "backup-dir", "", "local backup folder (or SQLITEADMIN_BACKUP_DIR)")
	f.StringVar(&o.StateDir, "state-dir", "", "private instance directory (default: per-database user state folder)")
	f.StringVar(&o.LogFile, "log-file", "", "background log (default: daemon.log in the instance directory)")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if f.NArg() != 0 {
		return o, errors.New("unexpected arguments; specify the database with --db")
	}
	set := map[string]bool{}
	f.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if o.EnvFile != "" {
		err := godotenv.Load(o.EnvFile)
		if err != nil && !(errors.Is(err, os.ErrNotExist) && !set["env-file"]) {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				return o, fmt.Errorf("load environment: %w", err)
			}
			// Dotenv parser errors may contain secrets from the input.
			return o, errors.New("invalid dotenv syntax; use KEY=value and check quotes")
		}
	}
	if !set["db"] {
		o.Database = os.Getenv("SQLITEADMIN_DB")
	}
	if !set["addr"] {
		o.Address = os.Getenv("SQLITEADMIN_ADDR")
	}
	if o.Address == "" {
		o.Address = "127.0.0.1:8081"
	}
	if !set["backup-dir"] {
		o.BackupDir = os.Getenv("SQLITEADMIN_BACKUP_DIR")
	}
	if !set["read-only"] && os.Getenv("SQLITEADMIN_READ_ONLY") != "" {
		var err error
		o.ReadOnly, err = strconv.ParseBool(os.Getenv("SQLITEADMIN_READ_ONLY"))
		if err != nil {
			return o, errors.New("SQLITEADMIN_READ_ONLY must be true or false")
		}
	}
	if o.Database == "" {
		return o, errors.New("a database is required: use --db /path/to/app.db or SQLITEADMIN_DB")
	}
	var err error
	o.Database, err = filepath.Abs(o.Database)
	if err != nil {
		return o, err
	}
	// Use the same instance for symlinks and relative/absolute paths.
	if real, e := filepath.EvalSymlinks(o.Database); e == nil {
		o.Database = real
	} else if !errors.Is(e, os.ErrNotExist) {
		return o, e
	}
	if o.BackupDir != "" {
		o.BackupDir, err = filepath.Abs(o.BackupDir)
		if err != nil {
			return o, err
		}
	}
	if o.StateDir == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" || !filepath.IsAbs(base) {
			home, e := os.UserHomeDir()
			if e != nil {
				return o, e
			}
			base = filepath.Join(home, ".local", "state")
		}
		hash := sha256.Sum256([]byte(o.Database))
		o.StateDir = filepath.Join(base, "sqliteadmin", fmt.Sprintf("%x", hash[:12]))
	}
	o.StateDir, err = filepath.Abs(o.StateDir)
	if err != nil {
		return o, err
	}
	if o.LogFile != "" {
		o.LogFile, err = filepath.Abs(o.LogFile)
		if err != nil {
			return o, err
		}
	}
	return o, nil
}
