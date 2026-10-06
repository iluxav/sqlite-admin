package sqliteadmin

import (
	"crypto/tls"
	"fmt"
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
	if ip := proxied.clientIP(r); ip != "10.0.0.1" {
		t.Errorf("behind a proxy: client address %q, want the last forwarded one, which the proxy appended", ip)
	}
	chain := &Admin{cfg: Config{ClientAddress: func(r *http.Request) string { return r.Header.Get("CF-Connecting-IP") }}}
	r.Header.Set("CF-Connecting-IP", "198.51.100.7")
	if ip := chain.clientIP(r); ip != "198.51.100.7" {
		t.Errorf("host-supplied client address: %q", ip)
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

func TestLoginLimiterStaysBounded(t *testing.T) {
	l := newLoginLimiter()
	for i := 0; i < 3*maxLoginEntries; i++ {
		l.fail(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255))
	}
	if len(l.m) > maxLoginEntries {
		t.Errorf("%d addresses remembered, cap %d", len(l.m), maxLoginEntries)
	}
	// A blocked address stays blocked while the table churns.
	for i := 0; i < maxLoginFailures; i++ {
		l.fail("203.0.113.1")
	}
	if !l.blocked("203.0.113.1") {
		t.Fatal("not blocked after repeated failures")
	}
}
