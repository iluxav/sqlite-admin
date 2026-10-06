package sqliteadmin

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// withConn runs fn on an admin connection and releases it before the caller
// renders (rendering takes its own connection for the sidebar).
func (a *Admin) withConn(r *http.Request, fn func(c *sql.Conn) error) error {
	c, err := a.conn(r.Context())
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c)
}

// ---- overview ----

type overviewData struct {
	Info     dbInfo
	FileSize int64
	Objects  []Object
}

func (a *Admin) overviewPage(w http.ResponseWriter, r *http.Request, s *session) error {
	var d overviewData
	err := a.withConn(r, func(c *sql.Conn) error {
		var err error
		if d.Info, err = loadDBInfo(r.Context(), c); err != nil {
			return err
		}
		d.Objects, err = listObjects(r.Context(), c)
		return err
	})
	if err != nil {
		return err
	}
	d.Info.Path, d.Info.Driver = a.cfg.Path, a.driver
	if st, err := os.Stat(a.cfg.Path); err == nil {
		d.FileSize = st.Size()
	}
	a.renderPage(w, r, http.StatusOK, "overview", "Overview", d)
	return nil
}

// ---- table data grid ----

type gridCol struct {
	Name      string
	Type      string
	PK        bool
	Generated bool
	SortURL   string
	Sorted    string // "asc", "desc" or ""
}

type gridCell struct {
	Table, Key, Col string
	Value           Value
	Editable        bool
	Dirty           bool
	Default         bool // staged insert leaves the column to its DEFAULT
	Title           string
}

type gridRow struct {
	Table    string
	Key      string
	Cells    []gridCell
	State    string // "", "deleted", "inserted"
	Editable bool
	ChangeID int
}

type tableData struct {
	Table     *Table
	Cols      []gridCol
	Rows      []gridRow
	Inserts   []gridRow
	Browse    browse
	Total     int64
	From, To  int64
	PrevURL   string
	NextURL   string
	Editable  bool
	Note      string
	FilterErr string
}

func browseURL(table string, b browse) string {
	q := url.Values{}
	if b.Page > 1 {
		q.Set("page", strconv.Itoa(b.Page))
	}
	if b.Sort != "" {
		q.Set("sort", b.Sort)
		if b.Desc {
			q.Set("dir", "desc")
		}
	}
	if b.Where != "" {
		q.Set("where", b.Where)
	}
	u := tableURL(table, "")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func parseBrowse(r *http.Request) browse {
	b := browse{Sort: r.FormValue("sort"), Desc: r.FormValue("dir") == "desc", Where: strings.TrimSpace(r.FormValue("where"))}
	b.Page, _ = strconv.Atoi(r.FormValue("page"))
	if b.Page < 1 {
		b.Page = 1
	}
	return b
}

// editable reports whether rows of t can be edited, and why not.
func (a *Admin) editable(t *Table) (bool, string) {
	switch {
	case a.cfg.ReadOnly:
		return false, "Read-only mode."
	case t.Type != "table":
		return false, "Views are read-only."
	case t.Virtual:
		return false, "Virtual tables are read-only here; use the SQL editor."
	case len(t.identity()) == 0:
		return false, "This table has no accessible rowid or non-null primary key, so rows cannot be addressed reliably."
	}
	return true, ""
}

func makeRow(t *Table, r dbRow, ov tableOverlay, editable bool) gridRow {
	row := gridRow{Table: t.Name, Key: r.Key, Editable: editable && r.Key != ""}
	if ov.deletes[r.Key] {
		row.State = "deleted"
	}
	ups := ov.updates[r.Key]
	for i, col := range t.Columns {
		cell := gridCell{Table: t.Name, Key: r.Key, Col: col.Name, Value: r.Values[i]}
		cell.Editable = row.Editable && row.State == "" && !col.Generated() && !cell.Value.IsBlob()
		if ch, ok := ups[col.Name]; ok {
			cell.Dirty, cell.Value = true, ch.New
			cell.Title = "was: " + ch.Old.Short()
		} else if cell.Value.Kind == 't' && len(cell.Value.S) > 120 {
			cell.Title = cell.Value.Display()
		}
		row.Cells = append(row.Cells, cell)
	}
	return row
}

func makeInsertRow(t *Table, ch change) gridRow {
	row := gridRow{Table: t.Name, State: "inserted", ChangeID: ch.ID, Editable: true}
	for _, col := range t.Columns {
		cell := gridCell{Table: t.Name, Col: col.Name, Default: true}
		for _, iv := range ch.Insert {
			if iv.Column == col.Name {
				cell.Value, cell.Default = iv.Value, false
			}
		}
		row.Cells = append(row.Cells, cell)
	}
	return row
}

func (a *Admin) tablePage(w http.ResponseWriter, r *http.Request, s *session) error {
	name := r.PathValue("name")
	d := tableData{Browse: parseBrowse(r)}
	if err := checkFragment("filter", d.Browse.Where); err != nil {
		d.FilterErr, d.Browse.Where = err.Error(), ""
	}
	err := a.withConn(r, func(c *sql.Conn) error {
		t, err := loadTable(r.Context(), c, name)
		if err != nil {
			return err
		}
		d.Table = t
		rows, total, err := fetchPage(r.Context(), c, t, d.Browse)
		if err != nil {
			if d.Browse.Where == "" {
				return err
			}
			d.FilterErr = err.Error()
		}
		d.Editable, d.Note = a.editable(t)
		ov := s.changes.overlay(t.Name)
		for _, row := range rows {
			d.Rows = append(d.Rows, makeRow(t, row, ov, d.Editable))
		}
		for _, ch := range ov.inserts {
			d.Inserts = append(d.Inserts, makeInsertRow(t, ch))
		}
		d.Total = total
		return nil
	})
	if err != nil {
		return err
	}
	t, b := d.Table, d.Browse
	for _, col := range t.Columns {
		gc := gridCol{Name: col.Name, Type: col.Type, PK: t.isPK(col.Name), Generated: col.Generated()}
		next := browse{Sort: col.Name, Where: b.Where}
		if b.Sort == col.Name {
			gc.Sorted = "asc"
			if b.Desc {
				gc.Sorted = "desc"
			} else {
				next.Desc = true
			}
		}
		gc.SortURL = browseURL(t.Name, next)
		d.Cols = append(d.Cols, gc)
	}
	if n := int64(len(d.Rows)); n > 0 {
		d.From = int64(b.Page-1)*pageSize + 1
		d.To = d.From + n - 1
	}
	if b.Page > 1 {
		prev := b
		prev.Page--
		d.PrevURL = browseURL(t.Name, prev)
	}
	if int64(b.Page)*pageSize < d.Total {
		next := b
		next.Page++
		d.NextURL = browseURL(t.Name, next)
	}
	w.Header().Add("Vary", "HX-Request, HX-Target, HX-History-Restore-Request")
	if isHTMX(r) && r.Header.Get("HX-Target") == "table-browser" && r.Header.Get("HX-History-Restore-Request") != "true" {
		a.execute(w, http.StatusOK, a.tmpl.pages["table"], "table-browser", pageData{DBName: path.Base(a.cfg.Path), Body: d})
	} else {
		a.renderPage(w, r, http.StatusOK, "table", t.Name, d)
	}
	return nil
}

// loadForEdit loads table name and checks its rows can be edited.
func (a *Admin) loadForEdit(r *http.Request, c *sql.Conn) (*Table, error) {
	t, err := loadTable(r.Context(), c, r.PathValue("name"))
	if err != nil {
		return nil, err
	}
	if ok, why := a.editable(t); !ok {
		return nil, badRequest("%s", why)
	}
	return t, nil
}

func (a *Admin) rowFragment(w http.ResponseWriter, r *http.Request, s *session) error {
	var row gridRow
	err := a.withConn(r, func(c *sql.Conn) error {
		t, err := loadTable(r.Context(), c, r.PathValue("name"))
		if err != nil {
			return err
		}
		dr, err := fetchRow(r.Context(), c, t, r.FormValue("key"))
		if err != nil {
			return err
		}
		editable, _ := a.editable(t)
		row = makeRow(t, dr, s.changes.overlay(t.Name), editable)
		return nil
	})
	if err != nil {
		return err
	}
	a.renderFragment(w, http.StatusOK, "row", row)
	return nil
}

type cellEditorData struct {
	Table, Key, Col string
	Text            string
	IsNull          bool
	Type            string
}

func (a *Admin) cellEditor(w http.ResponseWriter, r *http.Request, s *session) error {
	var d cellEditorData
	err := a.withConn(r, func(c *sql.Conn) error {
		t, err := a.loadForEdit(r, c)
		if err != nil {
			return err
		}
		col, ok := t.column(r.FormValue("col"))
		if !ok || col.Generated() {
			return badRequest("column %q cannot be edited", r.FormValue("col"))
		}
		dr, err := fetchRow(r.Context(), c, t, r.FormValue("key"))
		if err != nil {
			return err
		}
		v := dr.Values[slicesIndex(t, col.Name)]
		if ch, ok := s.changes.overlay(t.Name).updates[dr.Key][col.Name]; ok {
			v = ch.New
		}
		d = cellEditorData{Table: t.Name, Key: dr.Key, Col: col.Name, Text: v.EditText(), IsNull: v.IsNull(), Type: col.Type}
		return nil
	})
	if err != nil {
		return err
	}
	a.renderFragment(w, http.StatusOK, "cell-editor", d)
	return nil
}

func slicesIndex(t *Table, col string) int {
	for i, c := range t.Columns {
		if c.Name == col {
			return i
		}
	}
	return -1
}

func (a *Admin) stageCell(w http.ResponseWriter, r *http.Request, s *session) error {
	var row gridRow
	err := a.withConn(r, func(c *sql.Conn) error {
		t, err := a.loadForEdit(r, c)
		if err != nil {
			return err
		}
		col, ok := t.column(r.FormValue("col"))
		if !ok || col.Generated() {
			return badRequest("column %q cannot be edited", r.FormValue("col"))
		}
		dr, err := fetchRow(r.Context(), c, t, r.FormValue("key"))
		if err != nil {
			return err
		}
		old := dr.Values[slicesIndex(t, col.Name)]
		if old.IsBlob() {
			return badRequest("BLOB values can only be changed with the SQL editor")
		}
		nv := paramFor(col, r.FormValue("value"))
		// Returning to the stored text should discard a pending edit, including
		// numeric cells whose form value arrives as text rather than an integer.
		if !old.IsNull() && r.FormValue("value") == old.EditText() {
			nv = old
		}
		if r.FormValue("null") != "" {
			nv = nullValue
		}
		s.changes.stageUpdate(change{
			Table: t.Name, Key: dr.Key, KeyExprs: t.identity(),
			Column: col.Name, Old: old, New: nv,
		})
		row = makeRow(t, dr, s.changes.overlay(t.Name), true)
		return nil
	})
	if err != nil {
		return err
	}
	if r.FormValue("cell_only") == "1" {
		for _, cell := range row.Cells {
			if cell.Col == r.FormValue("col") {
				a.renderFragment(w, http.StatusOK, "cell", cell)
				break
			}
		}
	} else {
		a.renderFragment(w, http.StatusOK, "row", row)
	}
	a.oobBadge(w, s)
	return nil
}

func (a *Admin) toggleDeleteRow(w http.ResponseWriter, r *http.Request, s *session) error {
	var row gridRow
	err := a.withConn(r, func(c *sql.Conn) error {
		t, err := a.loadForEdit(r, c)
		if err != nil {
			return err
		}
		dr, err := fetchRow(r.Context(), c, t, r.FormValue("key"))
		if err != nil {
			return err
		}
		s.changes.toggleDelete(t, dr)
		row = makeRow(t, dr, s.changes.overlay(t.Name), true)
		return nil
	})
	if err != nil {
		return err
	}
	a.renderFragment(w, http.StatusOK, "row", row)
	a.oobBadge(w, s)
	return nil
}

type insertField struct {
	Index   int
	Col     Column
	Mode    string // "value", "null", "default"
	PK      bool
	Default string
}

type insertFormData struct {
	Table  string
	Fields []insertField
}

// insertColumns are the columns a row insert can set.
func insertColumns(t *Table) []Column {
	var out []Column
	for _, c := range t.Columns {
		if !c.Generated() {
			out = append(out, c)
		}
	}
	return out
}

func (a *Admin) insertForm(w http.ResponseWriter, r *http.Request, s *session) error {
	var d insertFormData
	err := a.withConn(r, func(c *sql.Conn) error {
		t, err := a.loadForEdit(r, c)
		if err != nil {
			return err
		}
		d.Table = t.Name
		for i, col := range insertColumns(t) {
			f := insertField{Index: i, Col: col, PK: t.isPK(col.Name), Mode: "value"}
			rowidAlias := len(t.PKCols) == 1 && f.PK && strings.EqualFold(col.Type, "INTEGER")
			if col.Default.Valid || rowidAlias || !col.NotNull {
				f.Mode = "default"
			}
			if col.Default.Valid {
				f.Default = col.Default.String
			}
			d.Fields = append(d.Fields, f)
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.renderFragment(w, http.StatusOK, "insert-form", d)
	return nil
}

func (a *Admin) stageInsert(w http.ResponseWriter, r *http.Request, s *session) error {
	return a.withConn(r, func(c *sql.Conn) error {
		t, err := a.loadForEdit(r, c)
		if err != nil {
			return err
		}
		var vals []insertValue
		for i, col := range insertColumns(t) {
			n := strconv.Itoa(i)
			switch r.FormValue("mode_" + n) {
			case "value":
				vals = append(vals, insertValue{col.Name, paramFor(col, r.FormValue("val_"+n))})
			case "null":
				vals = append(vals, insertValue{col.Name, nullValue})
			}
		}
		s.changes.addInsert(t.Name, vals)
		w.Header().Set("HX-Refresh", "true")
		return nil
	})
}

// ---- pending changes ----

type changeView struct {
	Items   []change
	Kind    changeKind
	Table   string
	Preview string
}

type changesData struct {
	Changes   []changeView
	Count     int
	Committed string
}

func (a *Admin) changesPage(w http.ResponseWriter, r *http.Request, s *session) error {
	d := changesData{Committed: r.FormValue("committed")}
	changes := s.changes.snapshot()
	d.Count = len(changes)
	for _, group := range groupChanges(changes) {
		st, err := group.statement()
		if err != nil {
			return err
		}
		d.Changes = append(d.Changes, changeView{Items: group, Kind: group[0].Kind, Table: group[0].Table, Preview: st.Preview})
	}
	a.renderPage(w, r, http.StatusOK, "changes", "Pending changes", d)
	return nil
}

func (a *Admin) commitChanges(w http.ResponseWriter, r *http.Request, s *session) error {
	n, err := a.commit(r.Context(), s.changes, s.user)
	if err != nil {
		return err
	}
	w.Header().Set("HX-Redirect", "/changes?committed="+strconv.Itoa(n))
	return nil
}

func (a *Admin) discardChanges(w http.ResponseWriter, r *http.Request, s *session) error {
	s.changes.clear()
	w.Header().Set("HX-Refresh", "true")
	return nil
}

func (a *Admin) removeChange(w http.ResponseWriter, r *http.Request, s *session) error {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		return badRequest("bad change id")
	}
	s.changes.remove(id)
	w.Header().Set("HX-Refresh", "true")
	return nil
}

// ---- schema ----

type schemaData struct {
	Table       *Table
	Indexes     []Index
	ForeignKeys []ForeignKey
	Triggers    []Object
	ColumnDefs  map[string]string
	CanAlter    bool
	CanIndex    bool
}

func (a *Admin) schemaPage(w http.ResponseWriter, r *http.Request, s *session) error {
	var d schemaData
	err := a.withConn(r, func(c *sql.Conn) error {
		ctx := r.Context()
		t, err := loadTable(ctx, c, r.PathValue("name"))
		if err != nil {
			return err
		}
		d.Table = t
		if t.Type == "table" {
			if d.Indexes, err = loadIndexes(ctx, c, t.Name); err != nil {
				return err
			}
			if d.ForeignKeys, err = loadForeignKeys(ctx, c, t.Name); err != nil {
				return err
			}
		}
		objs, err := listObjects(ctx, c)
		if err != nil {
			return err
		}
		for _, o := range objs {
			if o.Type == "trigger" && o.TblName == t.Name {
				d.Triggers = append(d.Triggers, o)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	t := d.Table
	d.CanAlter = !a.cfg.ReadOnly && t.Type == "table" && !t.Virtual
	d.CanIndex = d.CanAlter
	d.ColumnDefs = map[string]string{}
	if ct, err := parseCreateTable(t.SQL); err == nil {
		for _, col := range t.Columns {
			if i, ok := columnItem(ct, col.Name); ok {
				d.ColumnDefs[col.Name] = ct.items[i]
			}
		}
	}
	for _, col := range t.Columns {
		if _, ok := d.ColumnDefs[col.Name]; !ok {
			d.ColumnDefs[col.Name] = strings.TrimSpace(quoteIdent(col.Name) + " " + col.Type)
		}
	}
	a.renderPage(w, r, http.StatusOK, "schema", t.Name+" · schema", d)
	return nil
}

func (a *Admin) newTablePage(w http.ResponseWriter, r *http.Request, s *session) error {
	a.renderPage(w, r, http.StatusOK, "newtable", "New table", nil)
	return nil
}

type hiddenField struct{ Name, Value string }

type ddlConfirmData struct {
	Plan   *ddlPlan
	Fields []hiddenField
}

func (a *Admin) planFromRequest(r *http.Request, s *session) (*ddlPlan, error) {
	if err := r.ParseForm(); err != nil {
		return nil, badRequest("bad form: %v", err)
	}
	var p *ddlPlan
	err := a.withConn(r, func(c *sql.Conn) error {
		var err error
		p, err = a.planDDL(r.Context(), c, r.PostForm)
		return err
	})
	if err != nil {
		return nil, err
	}
	if p.Table != "" && s.changes.hasTable(p.Table) {
		return nil, badRequest("table %q has pending edits; commit or discard them first", p.Table)
	}
	return p, nil
}

func (a *Admin) ddlPreview(w http.ResponseWriter, r *http.Request, s *session) error {
	p, err := a.planFromRequest(r, s)
	if err != nil {
		return err
	}
	d := ddlConfirmData{Plan: p}
	keys := make([]string, 0, len(r.PostForm))
	for k := range r.PostForm {
		if k != "csrf_token" && k != "confirm" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range r.PostForm[k] {
			d.Fields = append(d.Fields, hiddenField{k, v})
		}
	}
	a.renderFragment(w, http.StatusOK, "ddl-confirm", d)
	return nil
}

func (a *Admin) ddlExec(w http.ResponseWriter, r *http.Request, s *session) error {
	p, err := a.planFromRequest(r, s)
	if err != nil {
		return err
	}
	if p.Confirm != "" && r.PostForm.Get("confirm") != p.Confirm {
		return badRequest("type %q to confirm", p.Confirm)
	}
	if err := a.execDDL(r.Context(), p, s.user); err != nil {
		return err
	}
	w.Header().Set("HX-Redirect", p.Redirect)
	return nil
}

// ---- SQL editor ----

type sqlData struct {
	SQL      string
	Name     string
	Snippets []snippet
	Results  *sqlResults
}

type sqlResults struct {
	Results []queryResult
	Warning string
}

type snippetsData struct {
	List     []snippet
	ReadOnly bool
}

func (a *Admin) sqlPage(w http.ResponseWriter, r *http.Request, s *session) error {
	var d sqlData
	err := a.withConn(r, func(c *sql.Conn) error {
		var err error
		if d.Snippets, err = listSnippets(r.Context(), c); err != nil {
			return err
		}
		if id := r.FormValue("snippet"); id != "" {
			n, _ := strconv.ParseInt(id, 10, 64)
			sn, err := getSnippet(r.Context(), c, n)
			if err != nil {
				return err
			}
			d.SQL, d.Name = sn.SQL, sn.Name
		}
		return nil
	})
	if err != nil {
		return err
	}
	if r.Method == http.MethodPost && d.SQL != "" {
		res, warn, err := a.runSQL(r.Context(), d.SQL, s.user)
		if err != nil {
			return err
		}
		d.Results = &sqlResults{res, warn}
	}
	a.renderPage(w, r, http.StatusOK, "sql", "SQL", d)
	return nil
}

func (a *Admin) sqlRun(w http.ResponseWriter, r *http.Request, s *session) error {
	res, warn, err := a.runSQL(r.Context(), r.FormValue("sql"), s.user)
	if err != nil {
		return err
	}
	a.renderFragment(w, http.StatusOK, "sql-results", &sqlResults{res, warn})
	return nil
}

func (a *Admin) renderSnippets(w http.ResponseWriter, r *http.Request, msg string) error {
	var list []snippet
	err := a.withConn(r, func(c *sql.Conn) error {
		var err error
		list, err = listSnippets(r.Context(), c)
		return err
	})
	if err != nil {
		return err
	}
	a.renderFragment(w, http.StatusOK, "snippets", snippetsData{list, a.cfg.ReadOnly})
	if msg != "" {
		fmt.Fprintf(w, `<div id="flash" hx-swap-oob="innerHTML">`)
		_ = a.tmpl.base.ExecuteTemplate(w, "flash", flash{Msg: msg})
		fmt.Fprint(w, `</div>`)
	}
	return nil
}

func (a *Admin) snippetSave(w http.ResponseWriter, r *http.Request, s *session) error {
	name := strings.TrimSpace(r.FormValue("name"))
	err := a.withConn(r, func(c *sql.Conn) error {
		return saveSnippet(r.Context(), c, name, r.FormValue("sql"))
	})
	if err != nil {
		return err
	}
	return a.renderSnippets(w, r, "Saved snippet “"+name+"”.")
}

func (a *Admin) snippetDelete(w http.ResponseWriter, r *http.Request, s *session) error {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return badRequest("bad snippet id")
	}
	if err := a.withConn(r, func(c *sql.Conn) error { return deleteSnippet(r.Context(), c, id) }); err != nil {
		return err
	}
	return a.renderSnippets(w, r, "")
}
