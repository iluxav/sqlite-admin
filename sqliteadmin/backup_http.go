package sqliteadmin

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type backupsPageData struct {
	LocalDir, Remote, ConfigError, ListError, Message string
	S3                                                bool
	Schedule                                          backupSchedule
	Entries                                           []backupEntry
	MaxUpload                                         int64
}

func (a *Admin) backupsPage(w http.ResponseWriter, r *http.Request, s *session) error {
	message := ""
	if r.FormValue("restored") == "1" {
		message = "Database restored. The previous database is saved in a local safety backup."
	}
	return a.renderBackups(w, r, message)
}

func (a *Admin) renderBackups(w http.ResponseWriter, r *http.Request, message string) error {
	m := a.backups
	d := backupsPageData{LocalDir: m.dir, S3: m.s3 != nil, ConfigError: m.s3Error, Schedule: m.state(), Message: message, MaxUpload: a.cfg.MaxRestoreBytes}
	entries, err := m.listLocal()
	if err != nil {
		d.ListError = "Cannot list local backups: " + err.Error()
	}
	d.Entries = entries
	if m.s3 != nil {
		d.Remote = "s3://" + m.s3.cfg.Bucket + "/" + m.s3.cfg.Prefix
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		remote, err := m.s3.list(ctx)
		cancel()
		if err != nil {
			d.ListError += " Cannot list S3 backups: " + err.Error()
		} else {
			d.Entries = append(d.Entries, remote...)
		}
	}
	sort.Slice(d.Entries, func(i, j int) bool { return d.Entries[i].Created.After(d.Entries[j].Created) })
	if isHTMX(r) {
		a.execute(w, http.StatusOK, a.tmpl.pages["backups"], "backup-panel", pageData{ReadOnly: a.cfg.ReadOnly, Body: d})
	} else {
		a.renderPage(w, r, http.StatusOK, "backups", "Backups", d)
	}
	return nil
}

func (a *Admin) backupCreate(w http.ResponseWriter, r *http.Request, s *session) error {
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.BackupTimeout)
	defer cancel()
	_, err := a.backups.create(ctx, r.FormValue("destination"), "manual")
	if err != nil {
		return err
	}
	return a.renderBackups(w, r, "Backup created.")
}

func (a *Admin) backupScheduleSave(w http.ResponseWriter, r *http.Request, s *session) error {
	minutes, err := strconv.Atoi(r.FormValue("minutes"))
	if err != nil {
		return badRequest("enter a valid interval in minutes")
	}
	err = a.backups.saveSchedule(r.FormValue("enabled") == "1", minutes, r.FormValue("destination"))
	if err != nil {
		return err
	}
	a.log.Info("backup schedule updated", "user", s.user, "enabled", r.FormValue("enabled") == "1", "minutes", minutes)
	return a.renderBackups(w, r, "Backup schedule saved.")
}

func (a *Admin) snapshotSource(ctx context.Context, source, name string, s *session) (string, func(), error) {
	noop := func() {}
	if source == "upload" {
		m := a.backups
		m.mu.Lock()
		u, ok := m.uploads[name]
		m.mu.Unlock()
		if !ok || u.Owner != s.id || time.Since(u.Created) > time.Hour {
			return "", noop, badRequest("upload expired; upload the backup again")
		}
		return u.File, noop, nil
	}
	if !validBackupName(name) {
		return "", noop, badRequest("invalid backup name")
	}
	if source == "local" {
		file, err := a.backups.localFile(name)
		return file, noop, err
	}
	if source != "s3" || a.backups.s3 == nil {
		return "", noop, badRequest("backup source is unavailable")
	}
	if err := os.MkdirAll(a.backups.dir, 0700); err != nil {
		return "", noop, err
	}
	f, err := os.CreateTemp(a.backups.dir, ".remote-restore-*")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { os.Remove(f.Name()) }
	err = a.backups.s3.get(ctx, name, f, a.cfg.MaxRestoreBytes)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return "", noop, err
	}
	return f.Name(), cleanup, nil
}

func (a *Admin) backupDownload(w http.ResponseWriter, r *http.Request, s *session) error {
	source, name := r.FormValue("source"), r.FormValue("name")
	if source == "upload" {
		return errNotFound
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.BackupTimeout)
	defer cancel()
	file, cleanup, err := a.snapshotSource(ctx, source, name, s)
	if err != nil {
		return err
	}
	defer cleanup()
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	http.ServeContent(w, r, name, info.ModTime(), f)
	return nil
}

type restoreData struct{ Source, Name, DisplayName, Database string }

func (a *Admin) backupRestoreForm(w http.ResponseWriter, r *http.Request, s *session) error {
	source, name := r.FormValue("source"), r.FormValue("name")
	if !validBackupName(name) || (source != "local" && source != "s3") {
		return badRequest("invalid backup")
	}
	if source == "local" {
		if _, err := a.backups.localFile(name); err != nil {
			return err
		}
	}
	if source == "s3" && a.backups.s3 == nil {
		return badRequest("S3 is not configured")
	}
	a.renderFragment(w, http.StatusOK, "restore-confirm", restoreData{Source: source, Name: name, DisplayName: name, Database: filepath.Base(a.cfg.Path)})
	return nil
}

type uploadedBackup struct {
	File, Name, Owner string
	Created           time.Time
}

func (a *Admin) backupUpload(w http.ResponseWriter, r *http.Request, s *session) error {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return badRequest("cannot read upload: %v", err)
	}
	defer r.MultipartForm.RemoveAll()
	input, header, err := r.FormFile("backup")
	if err != nil {
		return badRequest("choose a SQLite backup file")
	}
	defer input.Close()
	if header.Size > a.cfg.MaxRestoreBytes {
		return badRequest("backup exceeds the upload limit")
	}
	m := a.backups
	if err = os.MkdirAll(m.dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(m.dir, ".upload-*")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			os.Remove(f.Name())
		}
	}()
	n, err := io.Copy(f, io.LimitReader(input, a.cfg.MaxRestoreBytes+1))
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if n > a.cfg.MaxRestoreBytes {
		return badRequest("backup exceeds the upload limit")
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.BackupTimeout)
	defer cancel()
	if _, err = a.validateSnapshot(ctx, f.Name()); err != nil {
		return err
	}
	name := randomToken()
	m.mu.Lock()
	for key, u := range m.uploads {
		if u.Owner == s.id || time.Since(u.Created) > time.Hour {
			os.Remove(u.File)
			delete(m.uploads, key)
		}
	}
	m.uploads[name] = uploadedBackup{File: f.Name(), Name: header.Filename, Owner: s.id, Created: time.Now()}
	m.mu.Unlock()
	keep = true
	a.renderFragment(w, http.StatusOK, "restore-confirm", restoreData{Source: "upload", Name: name, DisplayName: header.Filename, Database: filepath.Base(a.cfg.Path)})
	return nil
}

// requireAuth holds databaseMu exclusively for this endpoint. SQLite itself
// provides the write transaction/rollback and coordinates host connections.
func (a *Admin) backupRestore(w http.ResponseWriter, r *http.Request, s *session) error {
	if r.FormValue("confirm") != filepath.Base(a.cfg.Path) {
		return badRequest("type the database filename to confirm restore")
	}
	a.sessions.mu.Lock()
	pending := false
	for _, se := range a.sessions.m {
		if time.Since(se.lastSeen) <= a.sessions.ttl && se.changes.count() != 0 {
			pending = true
			break
		}
	}
	a.sessions.mu.Unlock()
	if pending {
		return badRequest("commit or discard pending changes in every active session before restoring")
	}
	m := a.backups
	if !m.operation.TryLock() {
		return badRequest("another backup or restore is running; try again shortly")
	}
	defer m.operation.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.BackupTimeout)
	defer cancel()
	source, name := r.FormValue("source"), r.FormValue("name")
	file, cleanup, err := a.snapshotSource(ctx, source, name, s)
	if err != nil {
		return err
	}
	defer cleanup()
	pageSize, err := a.validateSnapshot(ctx, file)
	if err != nil {
		return err
	}
	c, err := a.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	var livePageSize int
	var journal string
	if err = c.QueryRowContext(ctx, "PRAGMA page_size").Scan(&livePageSize); err != nil {
		return err
	}
	if err = c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return err
	}
	if journal == "wal" && livePageSize != pageSize {
		return badRequest("this WAL database uses %d-byte pages; choose a backup with the same page size", livePageSize)
	}
	safety, err := m.snapshot(ctx, "local", "safety")
	if err != nil {
		return fmt.Errorf("restore cancelled; could not create the safety backup: %w", err)
	}
	if err = a.copyDatabase(ctx, c, file, true); err != nil {
		return fmt.Errorf("restore failed; safety backup %s is available: %w", safety.Name, err)
	}
	if source == "upload" {
		m.mu.Lock()
		delete(m.uploads, name)
		m.mu.Unlock()
		os.Remove(file)
	}
	a.log.Info("database restored", "user", s.user, "source", source, "backup", name, "safety_backup", safety.Name)
	// Schema, object navigation and all table views may have changed.
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/backups?restored=1")
		w.WriteHeader(http.StatusOK)
	} else {
		http.Redirect(w, r, "/backups?restored=1", http.StatusSeeOther)
	}
	return nil
}

func backupURL(route, source, name string) string {
	return "/backups/" + route + "?" + url.Values{"source": {source}, "name": {name}}.Encode()
}
