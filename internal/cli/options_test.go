package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func cleanEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"SQLITEADMIN_DB", "SQLITEADMIN_ADDR", "SQLITEADMIN_READ_ONLY", "SQLITEADMIN_BACKUP_DIR", "SQLITEADMIN_S3_ENDPOINT", "SQLITEADMIN_S3_BUCKET", "SQLITEADMIN_S3_ACCESS_KEY_ID", "SQLITEADMIN_S3_SECRET_ACCESS_KEY", "SQLITEADMIN_S3_REGION", "SQLITEADMIN_S3_PREFIX", "SQLITEADMIN_S3_SESSION_TOKEN"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOptionsPrecedence(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte("SQLITEADMIN_DB=from-file.db\nSQLITEADMIN_ADDR=127.0.0.1:8100\nSQLITEADMIN_READ_ONLY=true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SQLITEADMIN_ADDR", "127.0.0.1:8200")
	o, err := parseOptions([]string{"--env-file", env, "--db", filepath.Join(dir, "flags.db"), "--read-only=false", "--state-dir", dir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.Database != filepath.Join(dir, "flags.db") || o.Address != "127.0.0.1:8200" || o.ReadOnly {
		t.Fatalf("unexpected precedence: %+v", o)
	}
	o, err = parseOptions([]string{"--env-file=", "--db", filepath.Join(dir, "flags.db"), "--addr", "127.0.0.1:8300", "--state-dir", dir}, io.Discard)
	if err != nil || o.Address != "127.0.0.1:8300" || !o.ReadOnly {
		t.Fatalf("explicit address / env bool: %+v %v", o, err)
	}
}

func TestOptionsErrorsAndHelp(t *testing.T) {
	cleanEnv(t)
	dir := t.TempDir()
	if _, err := parseOptions([]string{"--env-file="}, io.Discard); err == nil {
		t.Fatal("database must be explicit")
	}
	if _, err := parseOptions([]string{"--env-file", filepath.Join(dir, "missing")}, io.Discard); err == nil {
		t.Fatal("explicit missing env file should fail")
	}
	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("PASSWORD='private-value"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseOptions([]string{"--env-file", bad}, io.Discard); err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatalf("dotenv error not safe: %v", err)
	}
	var out, stderr bytes.Buffer
	if code := Run([]string{"--help"}, &out, &stderr); code != 0 || !strings.Contains(out.String(), "start") || !strings.Contains(out.String(), "-db") {
		t.Fatalf("help: %d %s %s", code, &out, &stderr)
	}
}

func TestVersionDoesNotNeedConfiguration(t *testing.T) {
	for _, arg := range []string{"version", "--version"} {
		var out bytes.Buffer
		if code := Run([]string{arg}, &out, &out); code != 0 || !strings.HasPrefix(out.String(), "sqliteadmin "+Version+" ") {
			t.Fatalf("version: %d %s", code, &out)
		}
	}
}
