package sqliteadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

var backupNamePattern = regexp.MustCompile(`^(manual|scheduled|safety)-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[a-f0-9]{12}\.sqlite$`)

func validBackupName(name string) bool { return backupNamePattern.MatchString(name) }

type backupEntry struct {
	Name, Source string
	Size         int64
	Created      time.Time
}

type backupSchedule struct {
	Enabled     bool      `json:"enabled"`
	Minutes     int       `json:"minutes"`
	Destination string    `json:"destination"`
	NextRun     time.Time `json:"next_run"`
	LastRun     time.Time `json:"last_run"`
	LastSuccess time.Time `json:"last_success"`
	LastError   string    `json:"last_error,omitempty"`
}

type backupManager struct {
	a         *Admin
	dir       string
	s3        *s3Store
	s3Error   string
	mu        sync.Mutex
	operation sync.Mutex
	schedule  backupSchedule
	ctx       context.Context
	cancel    context.CancelFunc
	wake      chan struct{}
	done      chan struct{}
	uploads   map[string]uploadedBackup
}

func newBackupManager(a *Admin) (*backupManager, error) {
	dir := a.cfg.BackupDir
	if dir == "" {
		dir = os.Getenv("SQLITEADMIN_BACKUP_DIR")
	}
	if dir == "" {
		dir = a.cfg.Path + ".backups"
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	m := &backupManager{a: a, dir: dir, wake: make(chan struct{}, 1), done: make(chan struct{}), uploads: map[string]uploadedBackup{},
		schedule: backupSchedule{Minutes: 1440, Destination: "local"}}
	m.s3, m.s3Error = newS3Store(a.cfg)
	b, err := os.ReadFile(filepath.Join(dir, "schedule.json"))
	if err == nil {
		if err = json.Unmarshal(b, &m.schedule); err != nil {
			return nil, fmt.Errorf("sqliteadmin: invalid backup schedule: %w", err)
		}
		if err = validateSchedule(m.schedule); err != nil {
			return nil, fmt.Errorf("sqliteadmin: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m, nil
}

func validateSchedule(s backupSchedule) error {
	if s.Minutes < 1 || s.Minutes > 525600 {
		return errors.New("backup interval must be between 1 and 525600 minutes")
	}
	if s.Destination != "local" && s.Destination != "s3" {
		return errors.New("choose a local or S3 backup destination")
	}
	return nil
}

func (m *backupManager) state() backupSchedule { m.mu.Lock(); defer m.mu.Unlock(); return m.schedule }

func (m *backupManager) saveSchedule(enabled bool, minutes int, destination string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.schedule
	s.Enabled, s.Minutes, s.Destination = enabled, minutes, destination
	if err := validateSchedule(s); err != nil {
		return err
	}
	if enabled && destination == "s3" && m.s3 == nil {
		return errors.New("S3 is not configured")
	}
	s.NextRun = time.Time{}
	if enabled {
		s.NextRun = time.Now().UTC().Add(time.Duration(minutes) * time.Minute)
	}
	if err := m.persist(s); err != nil {
		return err
	}
	m.schedule = s
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return nil
}

func (m *backupManager) persist(s backupSchedule) error {
	if err := os.MkdirAll(m.dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(m.dir, ".schedule-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = json.NewEncoder(f).Encode(s); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(m.dir, "schedule.json")); err != nil {
		return err
	}
	return syncDirectory(m.dir)
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (m *backupManager) run() {
	defer close(m.done)
	for {
		s := m.state()
		wait := 24 * time.Hour
		if s.Enabled && !m.a.cfg.ReadOnly {
			wait = time.Until(s.NextRun)
			if wait < 0 {
				wait = 0
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-m.ctx.Done():
			timer.Stop()
			return
		case <-m.wake:
			timer.Stop()
			continue
		case <-timer.C:
		}
		m.runScheduled(time.Now().UTC())
	}
}

// Run only after HTTP requests and the scheduler have stopped. Removing an
// upload earlier could race an in-flight restore using that file.
func (m *backupManager) cleanupUploads() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, u := range m.uploads {
		os.Remove(u.File)
		delete(m.uploads, name)
	}
}

func (m *backupManager) runScheduled(now time.Time) {
	// Same lock ordering as HTTP operations. The scheduler does not hold mu
	// during I/O, so viewing status and changing a schedule remain responsive.
	m.a.databaseMu.RLock()
	defer m.a.databaseMu.RUnlock()
	m.mu.Lock()
	s := m.schedule
	if !s.Enabled || m.a.cfg.ReadOnly || now.Before(s.NextRun) || m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	s.LastRun = now
	s.NextRun = now.Add(time.Duration(s.Minutes) * time.Minute)
	// Persist the next deadline before starting: a crash cannot create a
	// runaway catch-up loop on restart. Missed periods produce one snapshot.
	if err := m.persist(s); err != nil {
		m.schedule.NextRun = now.Add(time.Minute)
		m.schedule.LastError = "Cannot save scheduler state: " + err.Error()
		m.mu.Unlock()
		return
	}
	m.schedule = s
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(m.ctx, m.a.cfg.BackupTimeout)
	defer cancel()
	_, err := m.create(ctx, s.Destination, "scheduled")
	m.mu.Lock()
	defer m.mu.Unlock()
	s = m.schedule // preserve any settings changed while the backup was running
	s.LastError = ""
	if err != nil {
		s.LastError = err.Error()
		m.a.log.Error("scheduled backup failed", "err", err)
	} else {
		s.LastSuccess = time.Now().UTC()
	}
	if saveErr := m.persist(s); saveErr != nil {
		s.LastError = "Cannot save scheduler result: " + saveErr.Error()
	}
	m.schedule = s
}

func (m *backupManager) create(ctx context.Context, destination, kind string) (backupEntry, error) {
	if !m.operation.TryLock() {
		return backupEntry{}, errors.New("another backup or restore is running; try again shortly")
	}
	defer m.operation.Unlock()
	return m.snapshot(ctx, destination, kind)
}

// snapshot assumes the operation lock is held. A remote failure leaves a
// complete local snapshot available to download and restore.
func (m *backupManager) snapshot(ctx context.Context, destination, kind string) (backupEntry, error) {
	var entry backupEntry
	if destination != "local" && destination != "s3" {
		return entry, badRequest("choose a backup destination")
	}
	if destination == "s3" && m.s3 == nil {
		return entry, badRequest("S3 is not configured")
	}
	if err := os.MkdirAll(m.dir, 0700); err != nil {
		return entry, err
	}
	f, err := os.CreateTemp(m.dir, ".snapshot-*")
	if err != nil {
		return entry, err
	}
	file := f.Name()
	f.Close()
	// The copy and its check open the file as a database, which leaves -wal and -shm beside it.
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			os.Remove(file + suffix)
		}
	}()
	c, err := m.a.conn(ctx)
	if err != nil {
		return entry, err
	}
	err = m.a.copyDatabase(ctx, c, file, false)
	c.Close()
	if err != nil {
		return entry, err
	}
	if _, err = m.a.validateSnapshot(ctx, file); err != nil {
		return entry, err
	}
	f, err = os.OpenFile(file, os.O_RDWR, 0600)
	if err != nil {
		return entry, err
	}
	err = f.Sync()
	info, statErr := f.Stat()
	f.Close()
	if err != nil {
		return entry, err
	}
	if statErr != nil {
		return entry, statErr
	}
	now := time.Now().UTC()
	name := kind + "-" + now.Format("20060102T150405.000000000Z") + "-" + randomToken()[:12] + ".sqlite"
	final := filepath.Join(m.dir, name)
	if err = os.Rename(file, final); err != nil {
		return entry, err
	}
	if err = syncDirectory(m.dir); err != nil {
		return entry, err
	}
	entry = backupEntry{Name: name, Source: destination, Size: info.Size(), Created: now}
	if destination == "s3" {
		if err = m.s3.put(ctx, name, final); err != nil {
			return entry, fmt.Errorf("S3 upload failed; local backup %s was kept: %w", name, err)
		}
		if err = os.Remove(final); err != nil {
			return entry, fmt.Errorf("uploaded to S3, but could not remove the local copy: %w", err)
		}
	}
	m.a.log.Info("backup created", "destination", destination, "name", name, "bytes", entry.Size)
	return entry, nil
}

func (m *backupManager) localFile(name string) (string, error) {
	if !validBackupName(name) {
		return "", badRequest("invalid backup name")
	}
	file := filepath.Join(m.dir, name)
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return "", errNotFound
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", badRequest("backup must be a regular file")
	}
	return file, nil
}

func (m *backupManager) listLocal() ([]backupEntry, error) {
	files, err := os.ReadDir(m.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []backupEntry
	for _, f := range files {
		if !validBackupName(f.Name()) || f.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := f.Info()
		if err != nil {
			return nil, err
		}
		if info.Mode().IsRegular() {
			out = append(out, backupEntry{Name: f.Name(), Source: "local", Size: info.Size(), Created: info.ModTime().UTC()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}
