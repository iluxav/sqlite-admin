package sqliteadmin

import (
	"context"
	"database/sql"
	"strings"
)

const pageSize = 100

// browse holds the grid's paging, sorting and filter state.
type browse struct {
	Page  int
	Sort  string
	Desc  bool
	Where string
}

// dbRow is one fetched row: its identity key and column values.
type dbRow struct {
	Key    string // encoded identity, "" when rows cannot be addressed
	Values []Value
}

// selectList selects the rowid first (when it identifies rows), then every
// column. Columns are selected as "+col" expressions so drivers see no declared
// type and return the stored value unconverted (e.g. no DATETIME -> time.Time).
func selectList(t *Table) string {
	var parts []string
	if t.usesRowID() {
		parts = append(parts, t.RowID)
	}
	for _, c := range t.Columns {
		parts = append(parts, "+"+quoteIdent(c.Name))
	}
	return strings.Join(parts, ", ")
}

func identityWhere(t *Table) string {
	ids := t.identity()
	conds := make([]string, len(ids))
	for i, id := range ids {
		conds[i] = id + " IS ?"
	}
	return strings.Join(conds, " AND ")
}

func readRows(t *Table, rows *sql.Rows) ([]dbRow, error) {
	n := len(t.Columns)
	offset := 0
	if t.usesRowID() {
		n++
		offset = 1
	}
	editable := len(t.identity()) > 0
	var out []dbRow
	for rows.Next() {
		vals, err := scanValues(rows, n)
		if err != nil {
			return nil, err
		}
		r := dbRow{Values: vals[offset:]}
		if editable {
			if t.usesRowID() {
				r.Key = encodeValues(vals[:1])
			} else {
				key := make([]Value, len(t.PKCols))
				for i, pk := range t.PKCols {
					for j, c := range t.Columns {
						if c.Name == pk {
							key[i] = r.Values[j]
						}
					}
				}
				r.Key = encodeValues(key)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// fetchPage returns one page of rows and the total matching row count.
func fetchPage(ctx context.Context, c *sql.Conn, t *Table, b browse) ([]dbRow, int64, error) {
	where := ""
	if b.Where != "" {
		where = " WHERE (" + b.Where + ")"
	}
	var total int64
	if err := c.QueryRowContext(ctx, "SELECT count(*) FROM "+quoteIdent(t.Name)+where).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := ""
	if _, ok := t.column(b.Sort); ok {
		order = " ORDER BY " + quoteIdent(b.Sort)
		if b.Desc {
			order += " DESC"
		}
	}
	q := "SELECT " + selectList(t) + " FROM " + quoteIdent(t.Name) + where + order + " LIMIT ? OFFSET ?"
	rows, err := c.QueryContext(ctx, q, pageSize, (b.Page-1)*pageSize)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out, err := readRows(t, rows)
	return out, total, err
}

// fetchRow returns the row identified by key, or errNotFound.
func fetchRow(ctx context.Context, c *sql.Conn, t *Table, key string) (dbRow, error) {
	vals, err := decodeValues(key)
	if err != nil {
		return dbRow{}, err
	}
	if len(vals) != len(t.identity()) {
		return dbRow{}, errNotFound
	}
	args := make([]any, len(vals))
	for i, v := range vals {
		args[i] = v.Arg()
	}
	rows, err := c.QueryContext(ctx, "SELECT "+selectList(t)+" FROM "+quoteIdent(t.Name)+" WHERE "+identityWhere(t)+" LIMIT 1", args...)
	if err != nil {
		return dbRow{}, err
	}
	defer rows.Close()
	out, err := readRows(t, rows)
	if err != nil {
		return dbRow{}, err
	}
	if len(out) == 0 {
		return dbRow{}, errNotFound
	}
	return out[0], nil
}
