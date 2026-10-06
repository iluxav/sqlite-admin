//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Let daemon children execute the real CLI entrypoint in this test binary.
func TestMain(m *testing.M) {
	if os.Getenv("SQLITEADMIN_CLI_TEST_HELPER") == "1" {
		os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func cliFixture(t *testing.T) (options, []string) {
	t.Helper()
	cleanEnv(t)
	t.Setenv("SQLITEADMIN_CLI_TEST_HELPER", "1")
	t.Setenv("SQLITEADMIN_USER", "test")
	t.Setenv("SQLITEADMIN_PASSWORD", "fixture-secret")
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE items(value TEXT); INSERT INTO items VALUES('unchanged')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	args := []string{"--env-file=", "--db", path, "--addr", "127.0.0.1:0", "--state-dir", filepath.Join(dir, "state")}
	o, err := parseOptions(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopDaemon(o) })
	return o, args
}

func invoke(command string, args []string) (int, string) {
	var out bytes.Buffer
	code := Run(append([]string{command}, args...), &out, &out)
	return code, out.String()
}

func TestDaemonLifecycle(t *testing.T) {
	o, args := cliFixture(t)
	args = append(args, "--read-only")
	if code, out := invoke("status", args); code != 3 {
		t.Fatalf("initial status: %d %s", code, out)
	}
	if code, out := invoke("start", args); code != 0 || !strings.Contains(out, "Started sqliteadmin") {
		t.Fatalf("start: %d %s", code, out)
	}
	state, err := daemonStatus(o)
	if err != nil || !state.ReadOnly || state.Database != o.Database || state.PID == os.Getpid() {
		t.Fatalf("state: %+v %v", state, err)
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + state.Address + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || !bytes.Contains(body, []byte("Open your workspace")) {
		t.Fatalf("server not ready: %d %v", response.StatusCode, err)
	}
	if code, out := invoke("start", args); code == 0 || !strings.Contains(out, "already running") {
		t.Fatalf("duplicate start: %d %s", code, out)
	}
	wrong := o
	wrong.Database = filepath.Join(t.TempDir(), "other.db")
	if _, err := controlRequest(wrong, http.MethodPost, "/stop"); err == nil {
		t.Fatal("wrong database stopped the instance")
	}
	if _, err := daemonStatus(o); err != nil {
		t.Fatalf("original instance was disrupted: %v", err)
	}
	for _, name := range []string{"daemon.log", "control.sock"} {
		st, err := os.Stat(filepath.Join(o.StateDir, name))
		if err != nil || st.Mode().Perm()&0077 != 0 {
			t.Fatalf("private %s: %v %v", name, st, err)
		}
	}
	logs, err := os.ReadFile(state.LogFile)
	if err != nil || bytes.Contains(logs, []byte("fixture-secret")) {
		t.Fatalf("log leaked credentials or missing: %v", err)
	}
	if code, out := invoke("stop", args); code != 0 {
		t.Fatalf("stop: %d %s", code, out)
	}
	if _, err := daemonStatus(o); !errors.Is(err, errNotRunning) {
		t.Fatalf("status after stop: %v", err)
	}
	if code, out := invoke("stop", args); code != 0 {
		t.Fatalf("idempotent stop: %d %s", code, out)
	}
	db, err := sql.Open("sqlite", o.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow("SELECT value FROM items").Scan(&value); err != nil || value != "unchanged" {
		t.Fatalf("database changed: %q %v", value, err)
	}
}

func TestDaemonStartupFailures(t *testing.T) {
	o, args := cliFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	occupied := append(append([]string{}, args...), "--addr", listener.Addr().String())
	if code, out := invoke("start", occupied); code == 0 || !strings.Contains(out, "listen") {
		t.Fatalf("occupied port reported success: %d %s", code, out)
	}
	if _, err := daemonStatus(o); !errors.Is(err, errNotRunning) {
		t.Fatalf("failed startup left an instance: %v", err)
	}
	t.Setenv("SQLITEADMIN_PASSWORD", "")
	if code, out := invoke("start", args); code == 0 || !strings.Contains(out, "password are required") {
		t.Fatalf("missing password: %d %s", code, out)
	}
	t.Setenv("SQLITEADMIN_PASSWORD", "fixture-secret")
	missing := filepath.Join(t.TempDir(), "missing.db")
	if code, out := invoke("start", append(append([]string{}, args...), "--db", missing, "--log-file", missing)); code == 0 || !strings.Contains(out, "log file must not be the database") {
		t.Fatalf("log file can create the database: %d %s", code, out)
	}
	if code, out := invoke("start", append(args, "--db", missing)); code == 0 {
		t.Fatalf("missing database reported success: %s", out)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("CLI created a missing database")
	}
}

func TestCrashRecoveryAndForegroundSignal(t *testing.T) {
	o, args := cliFixture(t)
	if code, out := invoke("start", args); code != 0 {
		t.Fatalf("start: %s", out)
	}
	state, err := daemonStatus(o)
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(state.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	process.Release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		locked, err := stateLocked(o.StateDir)
		if err != nil {
			t.Fatal(err)
		}
		if !locked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("crashed process kept its lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := daemonStatus(o); !errors.Is(err, errNotRunning) {
		t.Fatalf("stale socket treated as alive: %v", err)
	}
	if code, out := invoke("start", args); code != 0 {
		t.Fatalf("restart after crash: %s", out)
	}
	if err := stopDaemon(o); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, append([]string{"serve"}, args...)...)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	for {
		if _, err := daemonStatus(o); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("foreground failed to start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("foreground shutdown: %v %s", err, &logs)
	}
	if locked, _ := stateLocked(o.StateDir); locked {
		t.Fatal("foreground did not release the instance lock")
	}
}
