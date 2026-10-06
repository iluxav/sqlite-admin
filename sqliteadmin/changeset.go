package sqliteadmin

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
)

type changeKind string

const (
	chUpdate changeKind = "update"
	chInsert changeKind = "insert"
	chDelete changeKind = "delete"
)

// change is one staged, uncommitted edit.
type change struct {
	ID       int
	Kind     changeKind
	Table    string
	Key      string   // encoded row identity (update, delete)
	KeyExprs []string // identity expressions matching Key, e.g. ["\"id\""] or ["rowid"]
	Column   string   // update
	Old      Value    // update: value when first edited, verified at commit
	New      Value    // update
	Insert   []insertValue
	Before   []insertValue // delete: full row when staged, verified at commit
}

type insertValue struct {
	Column string
	Value  Value
}

// changeset holds a session's staged edits, applied together on commit.
type changeset struct {
	mu     sync.Mutex
	nextID int
	items  []*change
}

func (cs *changeset) add(c *change) {
	cs.nextID++
	c.ID = cs.nextID
	cs.items = append(cs.items, c)
}

// stageUpdate records a cell edit. Re-editing a cell keeps the original value;
// editing it back to the original drops the change.
func (cs *changeset) stageUpdate(c change) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for i, it := range cs.items {
		if it.Kind == chUpdate && it.Table == c.Table && it.Key == c.Key && it.Column == c.Column {
			if c.New.equal(it.Old) {
				cs.items = slices.Delete(cs.items, i, i+1)
			} else {
				it.New = c.New
			}
			return
		}
	}
	if c.New.equal(c.Old) {
		return
	}
	c.Kind = chUpdate
	cs.add(&c)
}

// toggleDelete stages a row delete, or unstages it if already staged.
// Staging a delete drops the row's pending updates. Reports whether the row
// is now staged for deletion.
func (cs *changeset) toggleDelete(t *Table, row dbRow) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for i, it := range cs.items {
		if it.Kind == chDelete && it.Table == t.Name && it.Key == row.Key {
			cs.items = slices.Delete(cs.items, i, i+1)
			return false
		}
	}
	cs.items = slices.DeleteFunc(cs.items, func(it *change) bool {
		return it.Kind == chUpdate && it.Table == t.Name && it.Key == row.Key
	})
	before := make([]insertValue, len(t.Columns))
	for i, col := range t.Columns {
		before[i] = insertValue{Column: col.Name, Value: row.Values[i]}
	}
	cs.add(&change{Kind: chDelete, Table: t.Name, Key: row.Key, KeyExprs: t.identity(), Before: before})
	return true
}

func (cs *changeset) addInsert(table string, vals []insertValue) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.add(&change{Kind: chInsert, Table: table, Insert: vals})
}

func (cs *changeset) remove(id int) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.items = slices.DeleteFunc(cs.items, func(it *change) bool { return it.ID == id })
}

func (cs *changeset) clear() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.items = nil
}

func (cs *changeset) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.items)
}

func (cs *changeset) hasTable(table string) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return slices.ContainsFunc(cs.items, func(it *change) bool { return it.Table == table })
}

// snapshot returns copies of the staged changes in staging order.
func (cs *changeset) snapshot() []change {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.snapshotLocked()
}

func (cs *changeset) snapshotLocked() []change {
	out := make([]change, len(cs.items))
	for i, it := range cs.items {
		out[i] = *it
	}
	return out
}

// tableOverlay is the pending state of one table, used to render the grid.
type tableOverlay struct {
	updates map[string]map[string]change // key -> column -> change
	deletes map[string]bool
	inserts []change
}

func (cs *changeset) overlay(table string) tableOverlay {
	o := tableOverlay{updates: map[string]map[string]change{}, deletes: map[string]bool{}}
	for _, it := range cs.snapshot() {
		if it.Table != table {
			continue
		}
		switch it.Kind {
		case chUpdate:
			if o.updates[it.Key] == nil {
				o.updates[it.Key] = map[string]change{}
			}
			o.updates[it.Key][it.Column] = it
		case chDelete:
			o.deletes[it.Key] = true
		case chInsert:
			o.inserts = append(o.inserts, it)
		}
	}
	return o
}

// stmt is one SQL statement with its bound arguments and a human-readable
// rendering with the arguments inlined.
type stmt struct {
	SQL       string
	Args      []any
	Preview   string
	ChangeID  int
	expectOne bool // UPDATE/DELETE must hit exactly one row, else conflict
}

type sqlBuilder struct {
	sql, preview strings.Builder
	args         []any
}

func (b *sqlBuilder) raw(s string) *sqlBuilder {
	b.sql.WriteString(s)
	b.preview.WriteString(s)
	return b
}

func (b *sqlBuilder) val(v Value) *sqlBuilder {
	b.sql.WriteString("?")
	b.preview.WriteString(v.Literal())
	b.args = append(b.args, v.Arg())
	return b
}

func (b *sqlBuilder) where(exprs []string, vals []Value) {
	b.raw(" WHERE ")
	for i, e := range exprs {
		if i > 0 {
			b.raw(" AND ")
		}
		b.raw(e + " IS ").val(vals[i])
	}
}

func (b *sqlBuilder) stmt(id int, expectOne bool) stmt {
	return stmt{SQL: b.sql.String(), Args: b.args, Preview: b.preview.String(), ChangeID: id, expectOne: expectOne}
}

func (c change) statement() (stmt, error) {
	var b sqlBuilder
	switch c.Kind {
	case chUpdate:
		return updateStatement([]change{c})
	case chDelete:
		keys, err := decodeValues(c.Key)
		if err != nil {
			return stmt{}, err
		}
		if len(keys) != len(c.KeyExprs) || len(c.Before) == 0 {
			return stmt{}, fmt.Errorf("missing row identity or original values")
		}
		exprs := slices.Clone(c.KeyExprs)
		for _, v := range c.Before {
			exprs = append(exprs, "+"+quoteIdent(v.Column)+" COLLATE BINARY")
			keys = append(keys, v.Value)
		}
		b.raw("DELETE FROM " + quoteIdent(c.Table))
		b.where(exprs, keys)
		return b.stmt(c.ID, true), nil
	case chInsert:
		b.raw("INSERT INTO " + quoteIdent(c.Table))
		if len(c.Insert) == 0 {
			b.raw(" DEFAULT VALUES")
			return b.stmt(c.ID, false), nil
		}
		cols := make([]string, len(c.Insert))
		for i, iv := range c.Insert {
			cols[i] = quoteIdent(iv.Column)
		}
		b.raw(" (" + strings.Join(cols, ", ") + ") VALUES (")
		for i, iv := range c.Insert {
			if i > 0 {
				b.raw(", ")
			}
			b.val(iv.Value)
		}
		b.raw(")")
		return b.stmt(c.ID, false), nil
	}
	return stmt{}, fmt.Errorf("unknown change kind %q", c.Kind)
}

// updateStatement changes all staged cells of one row atomically. In
// particular, changing a composite key must not invalidate later predicates
// or expose an intermediate key to constraints and triggers.
func updateStatement(changes []change) (stmt, error) {
	first := changes[0]
	keys, err := decodeValues(first.Key)
	if err != nil {
		return stmt{}, err
	}
	if len(keys) != len(first.KeyExprs) {
		return stmt{}, fmt.Errorf("invalid row identity")
	}
	var b sqlBuilder
	b.raw("UPDATE " + quoteIdent(first.Table) + " SET ")
	exprs := slices.Clone(first.KeyExprs)
	for i, c := range changes {
		if i > 0 {
			b.raw(", ")
		}
		b.raw(quoteIdent(c.Column) + " = ").val(c.New)
		exprs = append(exprs, "+"+quoteIdent(c.Column)+" COLLATE BINARY")
		keys = append(keys, c.Old)
	}
	b.where(exprs, keys)
	return b.stmt(first.ID, true), nil
}

type changeGroup []change

func (g changeGroup) statement() (stmt, error) {
	if g[0].Kind == chUpdate {
		return updateStatement(g)
	}
	return g[0].statement()
}

// Keep staging order, grouping each row's edits at its first staged update.
// The preview and commit use the same groups and SQL.
func groupChanges(changes []change) []changeGroup {
	type rowKey struct{ table, key string }
	updates := map[rowKey]int{}
	var groups []changeGroup
	for _, c := range changes {
		if c.Kind == chUpdate {
			key := rowKey{c.Table, c.Key}
			if i, ok := updates[key]; ok {
				groups[i] = append(groups[i], c)
				continue
			}
			updates[key] = len(groups)
		}
		groups = append(groups, changeGroup{c})
	}
	return groups
}

func buildStatements(changes []change) ([]stmt, error) {
	var out []stmt
	for _, group := range groupChanges(changes) {
		s, err := group.statement()
		if err != nil {
			return nil, fmt.Errorf("change #%d: %w", group[0].ID, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// commitError reports which staged change made the commit fail.
type commitError struct {
	ChangeID int
	Err      error
}

func (e *commitError) Error() string {
	return fmt.Sprintf("change #%d: %v — nothing was committed", e.ChangeID, e.Err)
}

// commit applies every staged change in one IMMEDIATE transaction. On any
// failure, including an UPDATE/DELETE whose row changed or vanished since it
// was staged, everything is rolled back and the changeset is kept.
func (a *Admin) commit(ctx context.Context, cs *changeset, who string) (int, error) {
	// The transaction must not be cut short by the client going away.
	ctx = context.WithoutCancel(ctx)
	c, err := a.conn(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	// Acquire the connection before locking: readers can hold a connection
	// while asking for an overlay. Holding mu while waiting for the pool
	// would deadlock with those readers. Keep the changeset stable until the
	// transaction finishes so concurrent commits cannot apply it twice.
	cs.mu.Lock()
	defer cs.mu.Unlock()
	changes := cs.snapshotLocked()
	if len(changes) == 0 {
		return 0, nil
	}
	stmts, err := buildStatements(changes)
	if err != nil {
		return 0, err
	}
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}
	rollback := func() { _, _ = c.ExecContext(ctx, "ROLLBACK") }
	for _, s := range stmts {
		res, err := c.ExecContext(ctx, s.SQL, s.Args...)
		if err != nil {
			rollback()
			return 0, &commitError{ChangeID: s.ChangeID, Err: err}
		}
		if s.expectOne {
			if n, err := res.RowsAffected(); err == nil && n != 1 {
				rollback()
				return 0, &commitError{ChangeID: s.ChangeID, Err: fmt.Errorf("the row was changed or deleted since you staged this edit (%d rows matched)", n)}
			}
		}
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		rollback()
		return 0, err
	}
	for _, s := range stmts {
		a.log.Info("commit", "user", who, "sql", s.SQL)
	}
	cs.items = nil
	return len(changes), nil
}
