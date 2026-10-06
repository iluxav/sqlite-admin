package sqliteadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// knownDrivers are the database/sql names registered by common SQLite drivers:
// "sqlite3" (mattn/go-sqlite3, ncruces/go-sqlite3) and "sqlite" (modernc.org/sqlite).
var knownDrivers = []string{"sqlite3", "sqlite"}

func detectDriver(name string) (string, error) {
	registered := sql.Drivers()
	if name != "" {
		if !slices.Contains(registered, name) {
			return "", fmt.Errorf("sqliteadmin: driver %q is not registered (registered: %v)", name, registered)
		}
		return name, nil
	}
	var found []string
	for _, d := range knownDrivers {
		if slices.Contains(registered, d) {
			found = append(found, d)
		}
	}
	switch len(found) {
	case 0:
		return "", errors.New(`sqliteadmin: no SQLite driver registered; import one in your app, e.g. _ "modernc.org/sqlite" or _ "github.com/mattn/go-sqlite3"`)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("sqliteadmin: several SQLite drivers registered %v; set Config.Driver", found)
	}
}

// conn takes a connection from the admin pool and applies the per-connection
// settings every admin operation relies on. Callers must Close it.
func (a *Admin) conn(ctx context.Context) (*sql.Conn, error) {
	c, err := a.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	queryOnly := "OFF"
	if a.cfg.ReadOnly {
		queryOnly = "ON"
	}
	for _, p := range []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA legacy_alter_table = OFF",
		"PRAGMA query_only = " + queryOnly,
	} {
		if _, err := c.ExecContext(ctx, p); err != nil {
			c.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return c, nil
}

var errNotFound = errors.New("not found")

// Object is a row of sqlite_master.
type Object struct {
	Type    string
	Name    string
	TblName string
	SQL     string
}

// listObjects returns user-visible schema objects; SQLite internals and the
// admin's own _sqliteadmin_* tables are hidden.
func listObjects(ctx context.Context, c *sql.Conn) ([]Object, error) {
	rows, err := c.QueryContext(ctx, `SELECT type, name, tbl_name, coalesce(sql, '') FROM sqlite_master
		WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name NOT LIKE '\_sqliteadmin\_%' ESCAPE '\'
		ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		var o Object
		if err := rows.Scan(&o.Type, &o.Name, &o.TblName, &o.SQL); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// Column is a row of PRAGMA table_xinfo.
type Column struct {
	CID     int
	Name    string
	Type    string
	NotNull bool
	Default sql.NullString
	PK      int
	Hidden  int
}

// Generated reports whether the column is a generated (computed) column.
func (c Column) Generated() bool { return c.Hidden == 2 || c.Hidden == 3 }

// affinity returns the SQLite type affinity of the declared type.
func (c Column) affinity() string {
	t := strings.ToUpper(c.Type)
	switch {
	case strings.Contains(t, "INT"):
		return "INTEGER"
	case strings.Contains(t, "CHAR"), strings.Contains(t, "CLOB"), strings.Contains(t, "TEXT"):
		return "TEXT"
	case t == "" || t == "ANY" || strings.Contains(t, "BLOB"):
		return "BLOB"
	case strings.Contains(t, "REAL"), strings.Contains(t, "FLOA"), strings.Contains(t, "DOUB"):
		return "REAL"
	default:
		return "NUMERIC"
	}
}

// Table describes a table or view and how its rows are identified.
type Table struct {
	Name    string
	Type    string // "table" or "view"
	SQL     string
	Virtual bool
	Columns []Column // visible columns (hidden virtual-table columns excluded)

	// RowID is the expression selecting the rowid ("rowid", "_rowid_" or
	// "oid", whichever is not shadowed by a column); empty when the table has
	// no rowid (views, WITHOUT ROWID tables).
	RowID  string
	PKCols []string
}

// identity prefers the rowid, since primary keys in ordinary SQLite tables
// may contain duplicate NULLs. Fall back to a non-null primary key when the
// rowid is unavailable (WITHOUT ROWID tables or shadowed rowid aliases).
func (t *Table) identity() []string {
	if t.Type != "table" || t.Virtual {
		return nil
	}
	if t.usesRowID() {
		return []string{t.RowID}
	}
	if len(t.PKCols) > 0 {
		out := make([]string, len(t.PKCols))
		for i, c := range t.PKCols {
			col, ok := t.column(c)
			if !ok || !col.NotNull {
				return nil
			}
			out[i] = quoteIdent(c)
		}
		return out
	}
	return nil
}

// usesRowID reports whether rows are identified by the rowid, which is then
// selected as an extra leading column.
func (t *Table) usesRowID() bool { return t.Type == "table" && !t.Virtual && t.RowID != "" }

func (t *Table) column(name string) (Column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

func (t *Table) isPK(name string) bool { return slices.Contains(t.PKCols, name) }

func loadTable(ctx context.Context, c *sql.Conn, name string) (*Table, error) {
	t := &Table{Name: name}
	err := c.QueryRowContext(ctx, `SELECT type, coalesce(sql, '') FROM sqlite_master WHERE name = ? AND type IN ('table', 'view')`, name).Scan(&t.Type, &t.SQL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Virtual = strings.HasPrefix(strings.Join(leadingWords(t.SQL, 3), " "), "CREATE VIRTUAL TABLE")

	rows, err := c.QueryContext(ctx, `SELECT cid, name, coalesce(type, ''), "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?)`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type pkCol struct {
		pos  int
		name string
	}
	var pks []pkCol
	for rows.Next() {
		var col Column
		if err := rows.Scan(&col.CID, &col.Name, &col.Type, &col.NotNull, &col.Default, &col.PK, &col.Hidden); err != nil {
			return nil, err
		}
		if col.Hidden == 1 { // hidden columns of virtual tables
			continue
		}
		t.Columns = append(t.Columns, col)
		if col.PK > 0 {
			pks = append(pks, pkCol{col.PK, col.Name})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	sort.Slice(pks, func(i, j int) bool { return pks[i].pos < pks[j].pos })
	for _, p := range pks {
		t.PKCols = append(t.PKCols, p.name)
	}

	if t.Type == "table" && !t.Virtual {
		for _, alias := range []string{"rowid", "_rowid_", "oid"} {
			if slices.ContainsFunc(t.Columns, func(c Column) bool { return strings.EqualFold(c.Name, alias) }) {
				continue
			}
			// WITHOUT ROWID tables reject every alias.
			r, err := c.QueryContext(ctx, "SELECT "+alias+" FROM "+quoteIdent(name)+" LIMIT 0")
			if err == nil {
				r.Close()
				t.RowID = alias
			}
			break
		}
	}
	return t, nil
}

// Index describes an index on a table.
type Index struct {
	Name    string
	Unique  bool
	Origin  string // "c" CREATE INDEX, "u" UNIQUE constraint, "pk" PRIMARY KEY
	Partial bool
	Columns []string
	SQL     string
}

func loadIndexes(ctx context.Context, c *sql.Conn, table string) ([]Index, error) {
	rows, err := c.QueryContext(ctx, `SELECT l.name, l."unique", l.origin, l.partial, coalesce(m.sql, '')
		FROM pragma_index_list(?) l LEFT JOIN sqlite_master m ON m.type = 'index' AND m.name = l.name
		ORDER BY l.name`, table)
	if err != nil {
		return nil, err
	}
	var out []Index
	for rows.Next() {
		var ix Index
		if err := rows.Scan(&ix.Name, &ix.Unique, &ix.Origin, &ix.Partial, &ix.SQL); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, ix)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		cols, err := c.QueryContext(ctx, `SELECT coalesce(name, '<expr>') FROM pragma_index_info(?) ORDER BY seqno`, out[i].Name)
		if err != nil {
			return nil, err
		}
		for cols.Next() {
			var n string
			if err := cols.Scan(&n); err != nil {
				cols.Close()
				return nil, err
			}
			out[i].Columns = append(out[i].Columns, n)
		}
		cols.Close()
	}
	return out, nil
}

// ForeignKey describes one (possibly composite) foreign key of a table.
type ForeignKey struct {
	Table    string
	From     []string
	To       []string
	OnUpdate string
	OnDelete string
}

func loadForeignKeys(ctx context.Context, c *sql.Conn, table string) ([]ForeignKey, error) {
	rows, err := c.QueryContext(ctx, `SELECT id, "table", "from", coalesce("to", ''), on_update, on_delete FROM pragma_foreign_key_list(?) ORDER BY id, seq`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ForeignKey
	last := -1
	for rows.Next() {
		var id int
		var parent, from, to, onUpdate, onDelete string
		if err := rows.Scan(&id, &parent, &from, &to, &onUpdate, &onDelete); err != nil {
			return nil, err
		}
		if id != last {
			out = append(out, ForeignKey{Table: parent, OnUpdate: onUpdate, OnDelete: onDelete})
			last = id
		}
		fk := &out[len(out)-1]
		fk.From = append(fk.From, from)
		fk.To = append(fk.To, to)
	}
	return out, rows.Err()
}

// dbInfo is shown on the overview page.
type dbInfo struct {
	Path          string
	Driver        string
	SQLiteVersion string
	JournalMode   string
	Encoding      string
	PageSize      int64
	PageCount     int64
	UserVersion   int64
	FreePages     int64
}

func (d dbInfo) Size() int64 { return d.PageSize * d.PageCount }

func loadDBInfo(ctx context.Context, c *sql.Conn) (dbInfo, error) {
	var d dbInfo
	for _, q := range []struct {
		sql  string
		dest any
	}{
		{"SELECT sqlite_version()", &d.SQLiteVersion},
		{"PRAGMA journal_mode", &d.JournalMode},
		{"PRAGMA encoding", &d.Encoding},
		{"PRAGMA page_size", &d.PageSize},
		{"PRAGMA page_count", &d.PageCount},
		{"PRAGMA user_version", &d.UserVersion},
		{"PRAGMA freelist_count", &d.FreePages},
	} {
		if err := c.QueryRowContext(ctx, q.sql).Scan(q.dest); err != nil {
			return d, fmt.Errorf("%s: %w", q.sql, err)
		}
	}
	return d, nil
}

// scanValues reads the current row into typed Values.
func scanValues(rows *sql.Rows, n int) ([]Value, error) {
	raw := make([]any, n)
	ptrs := make([]any, n)
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	out := make([]Value, n)
	for i, v := range raw {
		out[i] = valueFrom(v)
	}
	return out, nil
}
