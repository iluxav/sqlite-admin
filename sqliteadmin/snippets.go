package sqliteadmin

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Snippets live in the host database itself, in a table the UI hides.
const snippetsTable = "_sqliteadmin_snippets"

type snippet struct {
	ID        int64
	Name      string
	SQL       string
	UpdatedAt string
}

func snippetsExist(ctx context.Context, c *sql.Conn) (bool, error) {
	var n int
	err := c.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, snippetsTable).Scan(&n)
	return n > 0, err
}

func listSnippets(ctx context.Context, c *sql.Conn) ([]snippet, error) {
	if ok, err := snippetsExist(ctx, c); err != nil || !ok {
		return nil, err
	}
	rows, err := c.QueryContext(ctx, `SELECT id, name, sql, updated_at FROM `+snippetsTable+` ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []snippet
	for rows.Next() {
		var s snippet
		if err := rows.Scan(&s.ID, &s.Name, &s.SQL, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func getSnippet(ctx context.Context, c *sql.Conn, id int64) (snippet, error) {
	if ok, err := snippetsExist(ctx, c); err != nil {
		return snippet{}, err
	} else if !ok {
		return snippet{}, errNotFound
	}
	var s snippet
	err := c.QueryRowContext(ctx, `SELECT id, name, sql, updated_at FROM `+snippetsTable+` WHERE id = ?`, id).Scan(&s.ID, &s.Name, &s.SQL, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return s, errNotFound
	}
	return s, err
}

// saveSnippet creates the snippet, or replaces the SQL of the one with the
// same name.
func saveSnippet(ctx context.Context, c *sql.Conn, name, text string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("snippet name is required")
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("snippet SQL is empty")
	}
	if _, err := c.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+snippetsTable+` (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		sql TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
		updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	)`); err != nil {
		return err
	}
	_, err := c.ExecContext(ctx, `INSERT INTO `+snippetsTable+` (name, sql) VALUES (?, ?)
		ON CONFLICT (name) DO UPDATE SET sql = excluded.sql, updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')`, name, text)
	return err
}

func deleteSnippet(ctx context.Context, c *sql.Conn, id int64) error {
	if ok, err := snippetsExist(ctx, c); err != nil || !ok {
		return err
	}
	_, err := c.ExecContext(ctx, `DELETE FROM `+snippetsTable+` WHERE id = ?`, id)
	return err
}
