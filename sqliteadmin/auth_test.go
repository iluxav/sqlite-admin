package sqliteadmin

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAddressAndSecureCookieBehindAProxy(t *testing.T) {
	direct := &Admin{cfg: Config{}}
	proxied := &Admin{cfg: Config{BehindProxy: true}}

	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = "127.0.0.1:4321"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	r.Header.Set("X-Forwarded-Proto", "https")
	if ip := direct.clientIP(r); ip != "127.0.0.1" {
		t.Errorf("direct: forwarded header was trusted: %q", ip)
	}
	if direct.secure(r) {
		t.Error("direct: a plain request counted as https")
	}
	if ip := proxied.clientIP(r); ip != "203.0.113.9" {
		t.Errorf("behind a proxy: client address %q, want the first forwarded one", ip)
	}
	if !proxied.secure(r) {
		t.Error("behind a proxy: forwarded https not honoured")
	}

	plain := httptest.NewRequest(http.MethodPost, "/login", nil)
	plain.RemoteAddr = "127.0.0.1:4321"
	if ip := proxied.clientIP(plain); ip != "127.0.0.1" {
		t.Errorf("behind a proxy without the header: %q", ip)
	}
	if proxied.secure(plain) {
		t.Error("behind a proxy without the header counted as https")
	}
	over := httptest.NewRequest(http.MethodPost, "/login", nil)
	over.TLS = &tls.ConnectionState{}
	if !direct.secure(over) {
		t.Error("a TLS request is not secure")
	}

	w := httptest.NewRecorder()
	proxied.setSessionCookie(w, r, "s", 60)
	if c := w.Result().Cookies(); len(c) != 1 || !c[0].Secure || !c[0].HttpOnly {
		t.Errorf("session cookie behind a proxy: %+v", c)
	}
	t.Setenv(EnvBehindProxy, "yes")
	if !envBool(EnvBehindProxy) {
		t.Error("SQLITEADMIN_BEHIND_PROXY=yes was not read")
	}
}
