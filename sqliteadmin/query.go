package sqliteadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	maxResultRows = 1000
	queryTimeout  = 30 * time.Second
)

// queryResult is the outcome of one statement run from the SQL editor.
type queryResult struct {
	SQL       string
	Columns   []string
	Rows      [][]Value
	Truncated bool
	Changes   int64
	Duration  time.Duration
	Err       string
}

func (r queryResult) HasRows() bool { return len(r.Columns) > 0 }

// runSQL executes each statement of text in order on one connection,
// stopping at the first error. It returns the per-statement results and an
// optional warning.
func (a *Admin) runSQL(ctx context.Context, text, who string) ([]queryResult, string, error) {
	stmts := splitStatements(text)
	if len(stmts) == 0 {
		return nil, "", errors.New("nothing to run")
	}
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	c, err := a.conn(ctx)
	if err != nil {
		return nil, "", err
	}
	defer c.Close()

	var out []queryResult
	for _, s := range stmts {
		if a.cfg.ReadOnly {
			if w := leadingWords(s, 1); len(w) == 1 && (w[0] == "ATTACH" || w[0] == "VACUUM") {
				out = append(out, queryResult{SQL: s, Err: w[0] + " is not allowed in read-only mode"})
				break
			}
			// Re-assert before every statement so a "PRAGMA query_only = OFF"
			// earlier in the batch cannot unlock writes.
			if _, err := c.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
				return out, "", err
			}
		}
		r := runStatement(ctx, c, s)
		out = append(out, r)
		if r.Err != "" {
			break
		}
		if !a.cfg.ReadOnly && r.Changes > 0 {
			a.log.Info("sql", "user", who, "sql", s, "changes", r.Changes)
		}
	}

	// Never hand a connection with an open transaction back to the pool.
	warning := ""
	if _, err := c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); err == nil {
		warning = "A transaction was left open and has been rolled back. Put BEGIN and COMMIT in the same run."
	}
	return out, warning, nil
}

func runStatement(ctx context.Context, c *sql.Conn, s string) queryResult {
	r := queryResult{SQL: s}
	start := time.Now()
	fail := func(err error) queryResult {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("timed out after %s: %w", queryTimeout, err)
		}
		r.Err = err.Error()
		r.Duration = time.Since(start)
		return r
	}

	var before int64
	if err := c.QueryRowContext(ctx, "SELECT total_changes()").Scan(&before); err != nil {
		return fail(err)
	}
	// Query (not Exec) for everything: it returns rows for SELECT, PRAGMA and
	// RETURNING, and simply executes statements that have no result columns.
	rows, err := c.QueryContext(ctx, s)
	if err != nil {
		return fail(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return fail(err)
	}
	r.Columns = cols
	for rows.Next() {
		if len(cols) == 0 {
			continue
		}
		if len(r.Rows) == maxResultRows {
			r.Truncated = true
			break
		}
		vals, err := scanValues(rows, len(cols))
		if err != nil {
			return fail(err)
		}
		r.Rows = append(r.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	rows.Close()
	var after int64
	if err := c.QueryRowContext(ctx, "SELECT total_changes()").Scan(&after); err == nil {
		r.Changes = after - before
	}
	r.Duration = time.Since(start)
	return r
}
