package sqliteadmin

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

//go:embed templates static
var assets embed.FS

type templates struct {
	base  *template.Template // layout + partials, for fragments
	pages map[string]*template.Template
}

var funcs = template.FuncMap{
	"pathEsc":   url.PathEscape,
	"qesc":      url.QueryEscape,
	"browseURL": browseURL,
	"backupURL": backupURL,
	"backupKind": func(name string) string {
		switch {
		case strings.HasPrefix(name, "scheduled-"):
			return "Scheduled backup"
		case strings.HasPrefix(name, "safety-"):
			return "Safety backup"
		default:
			return "Manual backup"
		}
	},
	"backupTime": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("02 Jan 2006, 15:04 UTC")
	},
	"join": strings.Join,
	"tableURL": func(name string, suffix ...string) string {
		return tableURL(name, strings.Join(suffix, ""))
	},
	"add": func(a, b int) int { return a + b },
	"until": func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	},
	"plural": func(n any, one, many string) string {
		if fmt.Sprint(n) == "1" {
			return one
		}
		return many
	},
	"dur": func(d time.Duration) string {
		if d < time.Millisecond {
			return d.Round(time.Microsecond).String()
		}
		return d.Round(100 * time.Microsecond).String()
	},
	"originName": func(o string) string {
		switch o {
		case "pk":
			return "primary key"
		case "u":
			return "unique constraint"
		}
		return "CREATE INDEX"
	},
	"badge":        func(n int) badgeData { return badgeData{Count: n} },
	"snippetsData": func(list []snippet, ro bool) snippetsData { return snippetsData{list, ro} },
	"bytes": func(n int64) string {
		switch {
		case n >= 1<<30:
			return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
		case n >= 1<<20:
			return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
		case n >= 1<<10:
			return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
		}
		return fmt.Sprintf("%d B", n)
	},
}

func parseTemplates() (*templates, error) {
	base, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/partials.html")
	if err != nil {
		return nil, err
	}
	t := &templates{base: base, pages: map[string]*template.Template{}}
	files, err := fs.Glob(assets, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		clone, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := clone.ParseFS(assets, f); err != nil {
			return nil, err
		}
		t.pages[strings.TrimSuffix(path.Base(f), ".html")] = clone
	}
	return t, nil
}

func (a *Admin) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("POST /login", a.loginSubmit)

	h := func(pattern string, fn handlerFunc) { mux.Handle(pattern, a.requireAuth(a.wrap(fn))) }
	w := func(pattern string, fn handlerFunc) { h(pattern, a.writable(fn)) }

	mux.Handle("POST /logout", a.requireAuth(http.HandlerFunc(a.logout)))
	h("GET /{$}", a.overviewPage)
	h("GET /t/{name}", a.tablePage)
	h("GET /t/{name}/schema", a.schemaPage)
	h("GET /t/{name}/row", a.rowFragment)
	w("GET /t/{name}/cell", a.cellEditor)
	w("POST /t/{name}/cell", a.stageCell)
	w("POST /t/{name}/delete", a.toggleDeleteRow)
	w("GET /t/{name}/insert", a.insertForm)
	w("POST /t/{name}/insert", a.stageInsert)
	h("GET /changes", a.changesPage)
	w("POST /changes/commit", a.commitChanges)
	w("POST /changes/discard", a.discardChanges)
	w("POST /changes/{id}/remove", a.removeChange)
	h("GET /sql", a.sqlPage)
	h("POST /sql", a.sqlPage)
	h("POST /sql/run", a.sqlRun)
	w("POST /sql/snippets", a.snippetSave)
	w("POST /sql/snippets/{id}/delete", a.snippetDelete)
	w("GET /new-table", a.newTablePage)
	w("POST /ddl/preview", a.ddlPreview)
	w("POST /ddl/exec", a.ddlExec)
	h("GET /backups", a.backupsPage)
	h("GET /backups/download", a.backupDownload)
	w("POST /backups/create", a.backupCreate)
	w("POST /backups/schedule", a.backupScheduleSave)
	w("GET /backups/restore", a.backupRestoreForm)
	w("POST /backups/upload", a.backupUpload)
	w("POST /backups/restore", a.backupRestore)

	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(8 << 20)
		if r.URL.Path == "/backups/upload" {
			limit = a.cfg.MaxRestoreBytes + (1 << 20)
		}
		http.MaxBytesHandler(mux, limit).ServeHTTP(w, r)
	}))
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// handlerFunc is an authenticated handler; returned errors are rendered as a
// flash message (htmx requests) or an error page.
type handlerFunc func(w http.ResponseWriter, r *http.Request, s *session) error

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &httpError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func (a *Admin) wrap(fn handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := fn(w, r, sessionFrom(r))
		if err == nil {
			return
		}
		status := http.StatusInternalServerError
		var he *httpError
		switch {
		case errors.As(err, &he):
			status = he.status
		case errors.Is(err, errNotFound):
			status = http.StatusNotFound
		default:
			// SQLite errors are the useful message for an admin tool.
			status = http.StatusUnprocessableEntity
		}
		if status >= 500 {
			a.log.Error("request failed", "path", r.URL.Path, "err", err)
		}
		if isHTMX(r) {
			// Errors go to the flash area regardless of the request's target.
			w.Header().Set("HX-Retarget", "#flash")
			w.Header().Set("HX-Reswap", "innerHTML")
			a.renderFragment(w, http.StatusOK, "flash", flash{Error: true, Msg: err.Error()})
			return
		}
		a.renderPage(w, r, status, "error", "Error", err.Error())
	})
}

// writable rejects write handlers in read-only mode.
func (a *Admin) writable(fn handlerFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, s *session) error {
		if a.cfg.ReadOnly {
			return &httpError{http.StatusForbidden, "the admin UI is in read-only mode"}
		}
		return fn(w, r, s)
	}
}

type flash struct {
	Error bool
	Msg   string
}

// pageData is passed to every full page.
type pageData struct {
	Page     string
	Title    string
	CSRF     string
	User     string
	Pending  int
	ReadOnly bool
	DBName   string
	Objects  []Object
	Active   string
	Body     any
}

func (p pageData) Tables() []Object { return p.ofType("table") }
func (p pageData) Views() []Object  { return p.ofType("view") }

func (p pageData) ofType(t string) []Object {
	var out []Object
	for _, o := range p.Objects {
		if o.Type == t {
			out = append(out, o)
		}
	}
	return out
}

func (a *Admin) renderPage(w http.ResponseWriter, r *http.Request, status int, page, title string, body any) {
	data := pageData{Page: page, Title: title, ReadOnly: a.cfg.ReadOnly, DBName: path.Base(a.cfg.Path), Body: body}
	if s := sessionFrom(r); s != nil {
		data.CSRF, data.User, data.Pending = s.csrf, s.user, s.changes.count()
	}
	data.Active = r.PathValue("name")
	if c, err := a.conn(r.Context()); err == nil {
		data.Objects, _ = listObjects(r.Context(), c)
		c.Close()
	}
	t, ok := a.tmpl.pages[page]
	if !ok {
		http.Error(w, "unknown page "+page, http.StatusInternalServerError)
		return
	}
	a.execute(w, status, t, "layout", data)
}

func (a *Admin) renderLogin(w http.ResponseWriter, status int, msg string) {
	a.execute(w, status, a.tmpl.pages["login"], "login", msg)
}

func (a *Admin) renderFragment(w http.ResponseWriter, status int, name string, data any) {
	a.execute(w, status, a.tmpl.base, name, data)
}

func (a *Admin) execute(w http.ResponseWriter, status int, t *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		a.log.Error("template", "name", name, "err", err)
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// oobBadge renders the pending-changes badge for an out-of-band htmx swap.
// It is wrapped in <template> so it parses cleanly next to a <tr> response.
func (a *Admin) oobBadge(w http.ResponseWriter, s *session) {
	fmt.Fprint(w, "<template>")
	_ = a.tmpl.base.ExecuteTemplate(w, "pending-badge", badgeData{Count: s.changes.count(), OOB: true})
	_ = a.tmpl.base.ExecuteTemplate(w, "pending-state", badgeData{Count: s.changes.count(), OOB: true})
	fmt.Fprint(w, "</template>")
}

type badgeData struct {
	Count int
	OOB   bool
}
