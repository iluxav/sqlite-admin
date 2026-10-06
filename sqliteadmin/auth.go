package sqliteadmin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"sync"
	"time"
)

const sessionCookie = "sqliteadmin_session"

type session struct {
	writeMu  sync.Mutex // serialize mutations, including reading their initial database state
	id       string
	csrf     string
	user     string
	lastSeen time.Time
	changes  *changeset
}

type sessionStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*session
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, m: map[string]*session{}}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b)
}

func (s *sessionStore) create(user string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, se := range s.m {
		if now.Sub(se.lastSeen) > s.ttl {
			delete(s.m, id)
		}
	}
	se := &session{id: randomToken(), csrf: randomToken(), user: user, lastSeen: now, changes: &changeset{}}
	s.m[se.id] = se
	return se
}

func (s *sessionStore) get(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	se := s.m[id]
	if se == nil {
		return nil
	}
	if time.Since(se.lastSeen) > s.ttl {
		delete(s.m, id)
		return nil
	}
	se.lastSeen = time.Now()
	return se
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, id)
}

// loginLimiter blocks an IP for a while after repeated failed logins.
type loginLimiter struct {
	mu sync.Mutex
	m  map[string]*loginAttempts
}

type loginAttempts struct {
	failures     int
	first        time.Time
	blockedUntil time.Time
}

const (
	maxLoginFailures = 5
	loginWindow      = 15 * time.Minute
	loginBlock       = 15 * time.Minute
)

func newLoginLimiter() *loginLimiter { return &loginLimiter{m: map[string]*loginAttempts{}} }

func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	at := l.m[ip]
	return at != nil && time.Now().Before(at.blockedUntil)
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	at := l.m[ip]
	if at == nil || now.Sub(at.first) > loginWindow {
		at = &loginAttempts{first: now}
		l.m[ip] = at
	}
	at.failures++
	if at.failures >= maxLoginFailures {
		at.blockedUntil = now.Add(loginBlock)
		at.failures, at.first = 0, now
	}
}

func (l *loginLimiter) succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, ip)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// checkCredentials compares in constant time; hashing first hides lengths.
func (a *Admin) checkCredentials(user, pass string) bool {
	hu, hp := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
	wu, wp := sha256.Sum256([]byte(a.cfg.Username)), sha256.Sum256([]byte(a.cfg.Password))
	return subtle.ConstantTimeCompare(hu[:], wu[:])&subtle.ConstantTimeCompare(hp[:], wp[:]) == 1
}

func (a *Admin) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

type ctxKey struct{}

func sessionFrom(r *http.Request) *session {
	s, _ := r.Context().Value(ctxKey{}).(*session)
	return s
}

// requireAuth admits only requests with a live session; state-changing
// requests must also carry the session's CSRF token. (No Origin/Host
// comparison: it breaks behind reverse proxies that rewrite Host, and the
// token plus the SameSite=Strict cookie already stop cross-site requests.)
func (a *Admin) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var se *session
		if ck, err := r.Cookie(sessionCookie); err == nil {
			se = a.sessions.get(ck.Value)
		}
		if se == nil {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// Always take the database lock before session mutation locks. Restore
		// needs exclusivity across sessions, not just across connections.
		if r.Method == http.MethodPost && r.URL.Path == "/backups/restore" {
			a.databaseMu.Lock()
			defer a.databaseMu.Unlock()
		} else {
			a.databaseMu.RLock()
			defer a.databaseMu.RUnlock()
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("csrf_token")
			}
			if subtle.ConstantTimeCompare([]byte(token), []byte(se.csrf)) != 1 {
				http.Error(w, "invalid or missing CSRF token; reload the page", http.StatusForbidden)
				return
			}
			se.writeMu.Lock()
			defer se.writeMu.Unlock()
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, se)))
	})
}

func (a *Admin) loginPage(w http.ResponseWriter, r *http.Request) {
	a.renderLogin(w, http.StatusOK, "")
}

func (a *Admin) loginSubmit(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.limiter.blocked(ip) {
		a.renderLogin(w, http.StatusTooManyRequests, "Too many failed attempts. Try again in a few minutes.")
		return
	}
	user, pass := r.PostFormValue("username"), r.PostFormValue("password")
	if !a.checkCredentials(user, pass) {
		a.limiter.fail(ip)
		a.log.Warn("failed login", "ip", ip, "user", user)
		a.renderLogin(w, http.StatusUnauthorized, "Wrong username or password.")
		return
	}
	a.limiter.succeed(ip)
	se := a.sessions.create(user)
	a.setSessionCookie(w, r, se.id, 0)
	a.log.Info("login", "ip", ip, "user", user)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *Admin) logout(w http.ResponseWriter, r *http.Request) {
	if se := sessionFrom(r); se != nil {
		a.sessions.delete(se.id)
	}
	a.setSessionCookie(w, r, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
