package sqliteadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

// The two drivers return concrete backup types from their constructors. A
// checked reflection boundary lets the library use them without importing a
// second SQLite driver (or forcing CGO on the host). All stepping is typed.
type modernBackup interface {
	Step(int32) (bool, error) // true means more pages
	Finish() error
}
type mattnBackup interface {
	Step(int) (bool, error) // true means finished
	Finish() error
}

func copyConstructor(raw any, name string, args ...reflect.Value) (reflect.Value, bool) {
	m := reflect.ValueOf(raw).MethodByName(name)
	if !m.IsValid() || m.Type().NumIn() != len(args) || m.Type().NumOut() != 2 ||
		m.Type().Out(1) != reflect.TypeOf((*error)(nil)).Elem() {
		return reflect.Value{}, false
	}
	for i, arg := range args {
		if !arg.Type().AssignableTo(m.Type().In(i)) {
			return reflect.Value{}, false
		}
	}
	return m, true
}

func (a *Admin) copyDatabase(ctx context.Context, c *sql.Conn, file string, restore bool) error {
	if restore && a.cfg.RestoreCopy != nil {
		return a.cfg.RestoreCopy(ctx, c, file)
	}
	if !restore && a.cfg.BackupCopy != nil {
		return a.cfg.BackupCopy(ctx, c, file)
	}
	return c.Raw(func(raw any) error {
		name := "NewBackup"
		if restore {
			name = "NewRestore"
		}
		arg := reflect.ValueOf(fileURI(file, restore))
		if m, ok := copyConstructor(raw, name, arg); ok && m.Type().Out(0).Implements(reflect.TypeOf((*modernBackup)(nil)).Elem()) {
			out := m.Call([]reflect.Value{arg})
			if !out[1].IsNil() {
				return out[1].Interface().(error)
			}
			b := out[0].Interface().(modernBackup)
			return stepBackup(ctx, func() (bool, error) { more, err := b.Step(128); return !more, err }, b.Finish)
		}
		// mattn's API takes source and destination raw connections.
		other, err := sql.Open(a.driver, fileURI(file, restore))
		if err != nil {
			return err
		}
		defer other.Close()
		oc, err := other.Conn(ctx)
		if err != nil {
			return err
		}
		defer oc.Close()
		return oc.Raw(func(otherRaw any) error {
			src, dst := raw, otherRaw
			if restore {
				src, dst = otherRaw, raw
			}
			args := []reflect.Value{reflect.ValueOf("main"), reflect.ValueOf(src), reflect.ValueOf("main")}
			m, ok := copyConstructor(dst, "Backup", args...)
			if !ok || !m.Type().Out(0).Implements(reflect.TypeOf((*mattnBackup)(nil)).Elem()) {
				return errors.New("this SQLite driver needs Config.BackupCopy and Config.RestoreCopy adapters")
			}
			out := m.Call(args)
			if !out[1].IsNil() {
				return out[1].Interface().(error)
			}
			b := out[0].Interface().(mattnBackup)
			return stepBackup(ctx, func() (bool, error) { return b.Step(128) }, b.Finish)
		})
	})
}

func stepBackup(ctx context.Context, step func() (bool, error), finish func() error) (err error) {
	defer func() { err = errors.Join(err, finish()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		done, err := step()
		if err != nil {
			var coded interface{ Code() int }
			if !errors.As(err, &coded) || (coded.Code()&255 != 5 && coded.Code()&255 != 6) {
				return err
			}
		} else if done {
			return nil
		}
		// Also avoids a tight loop for mattn, which returns false/nil on BUSY.
		timer := time.NewTimer(time.Millisecond * 5)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func fileURI(file string, readOnly bool) string {
	abs, _ := filepath.Abs(file)
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	if readOnly {
		u.RawQuery = "mode=ro"
	}
	return u.String()
}

func (a *Admin) validateSnapshot(ctx context.Context, file string) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	var header [16]byte
	_, err = f.Read(header[:])
	f.Close()
	if err != nil || string(header[:]) != "SQLite format 3\x00" {
		return 0, errors.New("the file is not a SQLite database backup")
	}
	db, err := sql.Open(a.driver, fileURI(file, true))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "PRAGMA trusted_schema = OFF"); err != nil {
		return 0, err
	}
	var check string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return 0, fmt.Errorf("cannot validate backup: %w", err)
	}
	if check != "ok" {
		return 0, fmt.Errorf("backup failed SQLite integrity check: %s", check)
	}
	var size int
	err = db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size)
	return size, err
}
