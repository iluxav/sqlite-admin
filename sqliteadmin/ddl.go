package sqliteadmin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ddlPlan is a schema change, previewed before it runs. Plans are rebuilt
// from the submitted form at execution time; the browser never sends SQL.
type ddlPlan struct {
	Title      string
	Statements []string
	Notes      []string
	Rebuild    bool   // table rebuild: runs with foreign_keys OFF and legacy_alter_table ON
	Table      string // table that must have no pending edits
	Redirect   string
	Confirm    string // name the user must type to confirm (drop table)
	Danger     bool
}

var typeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_ ]*(\(\s*[+-]?\d+\s*(,\s*[+-]?\d+\s*)?\))?$`)

func checkType(t string) error {
	if t != "" && !typeRe.MatchString(t) {
		return fmt.Errorf("invalid column type %q", t)
	}
	return nil
}

func checkName(what, n string) error {
	if strings.TrimSpace(n) == "" {
		return errors.New(what + " is required")
	}
	if strings.HasPrefix(strings.ToLower(n), "sqlite_") || strings.HasPrefix(strings.ToLower(n), "_sqliteadmin_") {
		return fmt.Errorf("%s %q uses a reserved prefix", what, n)
	}
	return nil
}

func hasTopLevelComma(s string) bool {
	depth := 0
	for _, t := range tokenize(s) {
		if t.kind != tkPunct {
			continue
		}
		switch t.text {
		case "(":
			depth++
		case ")":
			depth--
		case ",":
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

func tableURL(name, suffix string) string { return "/t/" + url.PathEscape(name) + suffix }

// columnDef builds a simple column definition from form fields.
func columnDef(name, typ string, notNull, unique bool, def string) (string, error) {
	if err := checkName("column name", name); err != nil {
		return "", err
	}
	typ = strings.TrimSpace(typ)
	if err := checkType(typ); err != nil {
		return "", err
	}
	s := quoteIdent(name)
	if typ != "" {
		s += " " + typ
	}
	if notNull {
		s += " NOT NULL"
	}
	if unique {
		s += " UNIQUE"
	}
	if def = strings.TrimSpace(def); def != "" {
		if err := checkFragment("default", def); err != nil {
			return "", err
		}
		if hasTopLevelComma(def) {
			return "", errors.New("default must be a single expression")
		}
		s += " DEFAULT " + def
	}
	return s, nil
}

func (a *Admin) planDDL(ctx context.Context, c *sql.Conn, f url.Values) (*ddlPlan, error) {
	table := f.Get("table")
	needTable := func() (*Table, error) {
		t, err := loadTable(ctx, c, table)
		if err != nil {
			return nil, fmt.Errorf("table %q: %w", table, err)
		}
		if t.Type != "table" {
			return nil, fmt.Errorf("%q is a %s", table, t.Type)
		}
		return t, nil
	}
	needColumn := func(t *Table) (Column, error) {
		col, ok := t.column(f.Get("column"))
		if !ok {
			return Column{}, fmt.Errorf("column %q not found", f.Get("column"))
		}
		return col, nil
	}

	switch op := f.Get("op"); op {
	case "drop_table", "drop_view", "drop_index", "drop_trigger":
		kind := strings.TrimPrefix(op, "drop_")
		name := f.Get("name")
		var tbl string
		err := c.QueryRowContext(ctx, `SELECT tbl_name FROM sqlite_master WHERE type = ? AND name = ?`, kind, name).Scan(&tbl)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%s %q not found", kind, name)
		} else if err != nil {
			return nil, err
		}
		p := &ddlPlan{
			Title:      "Drop " + kind + " " + name,
			Statements: []string{"DROP " + strings.ToUpper(kind) + " " + quoteIdent(name)},
			Redirect:   tableURL(tbl, "/schema"),
			Danger:     true,
		}
		switch kind {
		case "table":
			p.Table, p.Confirm, p.Redirect = name, name, "/"
			p.Notes = append(p.Notes, "All rows, indexes and triggers of this table are deleted. This cannot be undone.")
		case "view":
			p.Redirect = "/"
		}
		return p, nil

	case "rename_table":
		if _, err := needTable(); err != nil {
			return nil, err
		}
		to := strings.TrimSpace(f.Get("new_name"))
		if err := checkName("new table name", to); err != nil {
			return nil, err
		}
		return &ddlPlan{
			Title:      "Rename table " + table,
			Statements: []string{"ALTER TABLE " + quoteIdent(table) + " RENAME TO " + quoteIdent(to)},
			Table:      table,
			Redirect:   tableURL(to, "/schema"),
		}, nil

	case "add_column":
		if _, err := needTable(); err != nil {
			return nil, err
		}
		def, err := columnDef(strings.TrimSpace(f.Get("name")), f.Get("type"), f.Get("notnull") != "", false, f.Get("default"))
		if err != nil {
			return nil, err
		}
		return &ddlPlan{
			Title:      "Add column to " + table,
			Statements: []string{"ALTER TABLE " + quoteIdent(table) + " ADD COLUMN " + def},
			Table:      table,
			Redirect:   tableURL(table, "/schema"),
		}, nil

	case "rename_column":
		t, err := needTable()
		if err != nil {
			return nil, err
		}
		col, err := needColumn(t)
		if err != nil {
			return nil, err
		}
		to := strings.TrimSpace(f.Get("new_name"))
		if err := checkName("new column name", to); err != nil {
			return nil, err
		}
		return &ddlPlan{
			Title:      "Rename column " + col.Name,
			Statements: []string{"ALTER TABLE " + quoteIdent(table) + " RENAME COLUMN " + quoteIdent(col.Name) + " TO " + quoteIdent(to)},
			Table:      table,
			Redirect:   tableURL(table, "/schema"),
		}, nil

	case "drop_column":
		t, err := needTable()
		if err != nil {
			return nil, err
		}
		col, err := needColumn(t)
		if err != nil {
			return nil, err
		}
		return &ddlPlan{
			Title:      "Drop column " + col.Name,
			Statements: []string{"ALTER TABLE " + quoteIdent(table) + " DROP COLUMN " + quoteIdent(col.Name)},
			Notes:      []string{"SQLite refuses to drop PRIMARY KEY, UNIQUE, indexed, foreign-key and generated-from columns; drop the index/constraint first or use “Edit definition”."},
			Table:      table,
			Redirect:   tableURL(table, "/schema"),
			Danger:     true,
		}, nil

	case "alter_column":
		t, err := needTable()
		if err != nil {
			return nil, err
		}
		col, err := needColumn(t)
		if err != nil {
			return nil, err
		}
		return planAlterColumn(ctx, c, t, col, strings.TrimSpace(f.Get("definition")))

	case "create_table":
		return planCreateTable(f)

	case "create_index":
		t, err := needTable()
		if err != nil {
			return nil, err
		}
		return planCreateIndex(t, f)
	}
	return nil, fmt.Errorf("unknown operation %q", f.Get("op"))
}

// columnItem returns the CREATE TABLE item defining column name.
func columnItem(ct *createTable, name string) (int, bool) {
	for i, item := range ct.items {
		if n, ok := itemColumn(item); ok && strings.EqualFold(n, name) {
			return i, true
		}
	}
	return 0, false
}

// planAlterColumn changes a column definition with SQLite's documented
// table rebuild: create the new table, copy rows, drop the old table, rename,
// then recreate its indexes and triggers.
func planAlterColumn(ctx context.Context, c *sql.Conn, t *Table, col Column, def string) (*ddlPlan, error) {
	if t.Virtual {
		return nil, errors.New("virtual tables cannot be altered")
	}
	if def == "" {
		return nil, errors.New("column definition is required")
	}
	if err := checkFragment("definition", def); err != nil {
		return nil, err
	}
	if hasTopLevelComma(def) {
		return nil, errors.New("definition must describe a single column")
	}
	if n, ok := itemColumn(def); !ok || !strings.EqualFold(n, col.Name) {
		return nil, fmt.Errorf("definition must start with the column name %s (use Rename to rename it)", quoteIdent(col.Name))
	}
	ct, err := parseCreateTable(t.SQL)
	if err != nil {
		return nil, err
	}
	i, ok := columnItem(ct, col.Name)
	if !ok {
		return nil, fmt.Errorf("column %q not found in the CREATE TABLE statement", col.Name)
	}
	ct.items[i] = def

	tmp := "_sqliteadmin_new_" + t.Name
	create := ct.build(tmp)

	oldCols := map[string]bool{}
	for _, oc := range t.Columns {
		if !oc.Generated() {
			oldCols[strings.ToLower(oc.Name)] = true
		}
	}
	var copyCols []string
	// Implicit rowids are part of a row's identity too. Copy the accessible
	// alias explicitly, including when a declared column shadows "rowid".
	if t.RowID != "" {
		copyCols = append(copyCols, t.RowID)
	}
	for _, item := range ct.items {
		if n, ok := itemColumn(item); ok && !itemGenerated(item) && oldCols[strings.ToLower(n)] {
			copyCols = append(copyCols, quoteIdent(n))
		}
	}
	cols := strings.Join(copyCols, ", ")

	stmts := []string{
		create,
		"INSERT INTO " + quoteIdent(tmp) + " (" + cols + ") SELECT " + cols + " FROM " + quoteIdent(t.Name),
		"DROP TABLE " + quoteIdent(t.Name),
		"ALTER TABLE " + quoteIdent(tmp) + " RENAME TO " + quoteIdent(t.Name),
	}

	rows, err := c.QueryContext(ctx, `SELECT sql FROM sqlite_master WHERE tbl_name = ? AND type IN ('index', 'trigger') AND sql IS NOT NULL ORDER BY type, name`, t.Name)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		stmts = append(stmts, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if containsWord(create, "AUTOINCREMENT") {
		var seq int64
		err := c.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = ?`, t.Name).Scan(&seq)
		if err == nil {
			n, s := quoteString(t.Name), strconv.FormatInt(seq, 10)
			stmts = append(stmts,
				"UPDATE sqlite_sequence SET seq = max(seq, "+s+") WHERE name = "+n,
				"INSERT INTO sqlite_sequence (name, seq) SELECT "+n+", "+s+" WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = "+n+")",
			)
		}
	}

	return &ddlPlan{
		Title:      "Change column " + col.Name,
		Statements: stmts,
		Notes: []string{
			"The table is rebuilt in one transaction: rows are copied into a new table, which replaces the old one; indexes and triggers are recreated.",
			"Runs with foreign_keys OFF and legacy_alter_table ON, then checks PRAGMA foreign_key_check before committing.",
		},
		Rebuild:  true,
		Table:    t.Name,
		Redirect: tableURL(t.Name, "/schema"),
		Danger:   true,
	}, nil
}

var colFieldRe = regexp.MustCompile(`^c(\d+)_name$`)

func planCreateTable(f url.Values) (*ddlPlan, error) {
	name := strings.TrimSpace(f.Get("name"))
	if err := checkName("table name", name); err != nil {
		return nil, err
	}
	var idx []int
	for k := range f {
		if m := colFieldRe.FindStringSubmatch(k); m != nil {
			n, _ := strconv.Atoi(m[1])
			idx = append(idx, n)
		}
	}
	sort.Ints(idx)
	var items, pks []string
	for _, i := range idx {
		p := "c" + strconv.Itoa(i) + "_"
		cn := strings.TrimSpace(f.Get(p + "name"))
		if cn == "" {
			continue
		}
		def, err := columnDef(cn, f.Get(p+"type"), f.Get(p+"notnull") != "", f.Get(p+"unique") != "", f.Get(p+"default"))
		if err != nil {
			return nil, err
		}
		items = append(items, def)
		if f.Get(p+"pk") != "" {
			pks = append(pks, quoteIdent(cn))
		}
	}
	if len(items) == 0 {
		return nil, errors.New("add at least one column")
	}
	if len(pks) > 0 {
		items = append(items, "PRIMARY KEY ("+strings.Join(pks, ", ")+")")
	}
	var opts []string
	if f.Get("without_rowid") != "" {
		opts = append(opts, "WITHOUT ROWID")
	}
	if f.Get("strict") != "" {
		opts = append(opts, "STRICT")
	}
	ct := &createTable{items: items, suffix: strings.Join(opts, ", ")}
	return &ddlPlan{
		Title:      "Create table " + name,
		Statements: []string{ct.build(name)},
		Redirect:   tableURL(name, "/schema"),
	}, nil
}

func planCreateIndex(t *Table, f url.Values) (*ddlPlan, error) {
	var cols, names []string
	for i := 1; i <= 4; i++ {
		cn := f.Get("col" + strconv.Itoa(i))
		if cn == "" {
			continue
		}
		if _, ok := t.column(cn); !ok {
			return nil, fmt.Errorf("column %q not found", cn)
		}
		cols = append(cols, quoteIdent(cn))
		names = append(names, cn)
	}
	if len(cols) == 0 {
		return nil, errors.New("pick at least one column")
	}
	name := strings.TrimSpace(f.Get("name"))
	if name == "" {
		name = "idx_" + t.Name + "_" + strings.Join(names, "_")
	}
	if err := checkName("index name", name); err != nil {
		return nil, err
	}
	s := "CREATE "
	if f.Get("unique") != "" {
		s += "UNIQUE "
	}
	s += "INDEX " + quoteIdent(name) + " ON " + quoteIdent(t.Name) + " (" + strings.Join(cols, ", ") + ")"
	if where := strings.TrimSpace(f.Get("where")); where != "" {
		if err := checkFragment("WHERE", where); err != nil {
			return nil, err
		}
		s += " WHERE " + where
	}
	return &ddlPlan{
		Title:      "Create index on " + t.Name,
		Statements: []string{s},
		Redirect:   tableURL(t.Name, "/schema"),
	}, nil
}

// execDDL runs a plan in one IMMEDIATE transaction.
func (a *Admin) execDDL(ctx context.Context, p *ddlPlan, who string) error {
	ctx = context.WithoutCancel(ctx)
	c, err := a.conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if p.Rebuild {
		// Both pragmas are no-ops inside a transaction, so set them first.
		for _, s := range []string{"PRAGMA foreign_keys = OFF", "PRAGMA legacy_alter_table = ON"} {
			if _, err := c.ExecContext(ctx, s); err != nil {
				return err
			}
		}
		defer func() {
			_, _ = c.ExecContext(ctx, "PRAGMA legacy_alter_table = OFF")
			_, _ = c.ExecContext(ctx, "PRAGMA foreign_keys = ON")
		}()
	}
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	rollback := func() { _, _ = c.ExecContext(ctx, "ROLLBACK") }
	for _, s := range p.Statements {
		if _, err := c.ExecContext(ctx, s); err != nil {
			rollback()
			return fmt.Errorf("%w\n\nwhile running:\n%s", err, s)
		}
	}
	if p.Rebuild {
		if err := foreignKeyCheck(ctx, c); err != nil {
			rollback()
			return err
		}
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		rollback()
		return err
	}
	for _, s := range p.Statements {
		a.log.Info("schema change", "user", who, "sql", s)
	}
	return nil
}

func foreignKeyCheck(ctx context.Context, c *sql.Conn) error {
	rows, err := c.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	var problems []string
	for rows.Next() && len(problems) < 5 {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return err
		}
		problems = append(problems, fmt.Sprintf("%s (rowid %v) → %s", table, rowid.Int64, parent))
	}
	if len(problems) > 0 {
		return fmt.Errorf("the change would violate foreign keys: %s — nothing was changed", strings.Join(problems, "; "))
	}
	return rows.Err()
}
