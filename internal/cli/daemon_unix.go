//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func prepareState(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("state directory %s must be owned by you, mode 0700, and not a symlink", dir)
	}
	return nil
}

func acquireLock(dir string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(dir, "instance.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.New("an instance is already running or starting; use status or stop for this database")
		}
		return nil, err
	}
	return file, nil
}

func releaseLock(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
	// Never unlink the lock file: other processes may already have it open.
}

func stateLocked(dir string) (bool, error) {
	file, err := os.OpenFile(filepath.Join(dir, "instance.lock"), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return false, nil
}

func serveManaged(o options, ready func(instance) error) error {
	if err := prepareState(o.StateDir); err != nil {
		return err
	}
	lock, err := acquireLock(o.StateDir)
	if err != nil {
		return err
	}
	defer releaseLock(lock)
	stop := make(chan struct{}, 1)
	var control *http.Server
	defer func() {
		if control != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = control.Shutdown(ctx)
		}
	}()
	return serve(o, stop, func(state instance) error {
		path := filepath.Join(o.StateDir, "control.sock")
		if st, err := os.Lstat(path); err == nil {
			if st.Mode()&os.ModeSocket == 0 {
				return fmt.Errorf("control path %s is not a socket", path)
			}
			if err := os.Remove(path); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			return fmt.Errorf("create control socket (try a shorter --state-dir if the path is too long): %w", err)
		}
		if err := os.Chmod(path, 0600); err != nil {
			listener.Close()
			return err
		}
		mux := http.NewServeMux()
		respond := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state)
		}
		mux.HandleFunc("GET /status", respond)
		mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Sqliteadmin-Database") != state.Database {
				http.Error(w, "state directory belongs to a different database", http.StatusConflict)
				return
			}
			respond(w, r)
			select {
			case stop <- struct{}{}:
			default:
			}
		})
		control = &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 3 * time.Second}
		go func() { _ = control.Serve(listener) }()
		if ready != nil {
			return ready(state)
		}
		return nil
	})
}

type startupReply struct {
	State instance `json:"state"`
	Error string   `json:"error,omitempty"`
}

func daemonChild(o options) error {
	// Only start creates this inherited readiness pipe. It never holds secrets.
	file := os.NewFile(3, "startup")
	if file == nil {
		return errors.New("missing daemon startup pipe")
	}
	st, err := file.Stat()
	if err != nil || st.Mode()&os.ModeNamedPipe == 0 {
		file.Close()
		return errors.New("missing daemon startup pipe")
	}
	defer file.Close()
	notified := false
	err = serveManaged(o, func(state instance) error {
		if err := json.NewEncoder(file).Encode(startupReply{State: state}); err != nil {
			return err
		}
		notified = true
		return file.Close()
	})
	if err != nil && !notified {
		_ = json.NewEncoder(file).Encode(startupReply{Error: err.Error()})
	}
	return err
}

func startDaemon(o options) (instance, error) {
	if err := prepareState(o.StateDir); err != nil {
		return instance{}, err
	}
	if o.LogFile == "" {
		o.LogFile = filepath.Join(o.StateDir, "daemon.log")
	}
	if filepath.Clean(o.LogFile) == filepath.Clean(o.Database) {
		return instance{}, errors.New("log file must not be the database")
	}
	if db, err := os.Stat(o.Database); err == nil {
		if log, err := os.Stat(o.LogFile); err == nil && os.SameFile(db, log) {
			return instance{}, errors.New("log file must not be the database")
		}
	}
	log, err := os.OpenFile(o.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return instance{}, err
	}
	defer log.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		return instance{}, err
	}
	defer reader.Close()
	defer writer.Close()
	exe, err := os.Executable()
	if err != nil {
		return instance{}, err
	}
	// All paths are absolute; the child keeps the working directory for other
	// relative environment settings. Configuration is already in its environment.
	cmd := exec.Command(exe, "_daemon", "--db", o.Database, "--addr", o.Address,
		"--read-only="+strconv.FormatBool(o.ReadOnly), "--backup-dir", o.BackupDir,
		"--state-dir", o.StateDir, "--log-file", o.LogFile, "--env-file=")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.ExtraFiles = []*os.File{writer}
	if err := cmd.Start(); err != nil {
		return instance{}, err
	}
	writer.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	replies := make(chan startupReply, 1)
	go func() {
		var reply startupReply
		if err := json.NewDecoder(io.LimitReader(reader, 64<<10)).Decode(&reply); err != nil {
			reply.Error = "daemon exited before reporting readiness; see " + o.LogFile
		}
		replies <- reply
	}()
	select {
	case reply := <-replies:
		if reply.Error == "" {
			return reply.State, nil
		}
		// The child is ours, and has not reported readiness; no PID-file signalling.
		_ = cmd.Process.Kill()
		<-exited
		return instance{}, errors.New(reply.Error)
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		return instance{}, fmt.Errorf("startup timed out; see %s", o.LogFile)
	}
}

func controlRequest(o options, method, path string) (instance, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(o.StateDir, "control.sock"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	req, err := http.NewRequest(method, "http://sqliteadmin"+path, nil)
	if err != nil {
		return instance{}, err
	}
	req.Header.Set("X-Sqliteadmin-Database", o.Database)
	response, err := client.Do(req)
	if err != nil {
		locked, lockErr := stateLocked(o.StateDir)
		if lockErr != nil {
			return instance{}, lockErr
		}
		if !locked {
			return instance{}, errNotRunning
		}
		return instance{}, fmt.Errorf("instance is starting, stopping, or its control socket is unavailable: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return instance{}, fmt.Errorf("control request: %s", response.Status)
	}
	var state instance
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&state); err != nil {
		return state, err
	}
	if state.Database != o.Database {
		return state, errors.New("state directory belongs to a different database")
	}
	return state, nil
}

func daemonStatus(o options) (instance, error) { return controlRequest(o, http.MethodGet, "/status") }

func stopDaemon(o options) error {
	// Check identity before asking the socket to stop anything.
	if _, err := daemonStatus(o); err != nil {
		return err
	}
	if _, err := controlRequest(o, http.MethodPost, "/stop"); err != nil {
		return err
	}
	deadline := time.NewTimer(35 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		locked, err := stateLocked(o.StateDir)
		if err != nil {
			return err
		}
		if !locked {
			return nil
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return errors.New("still stopping; check status and the log")
		}
	}
}
