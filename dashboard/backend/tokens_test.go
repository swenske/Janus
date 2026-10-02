package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/swenske/Janus/dashboard/backend/internal/auth"
)

func TestBearerTokens(t *testing.T) {
	dir := t.TempDir()
	authStore, err := auth.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := authStore.Setup("the-admin-password"); err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.OpenTokens(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{auth: authStore, tokens: tokens, loginLimiter: auth.NewLoginLimiter()}
	secret, tok, err := tokens.Create("terraform", 0)
	if err != nil {
		t.Fatal(err)
	}
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	do := func(h http.HandlerFunc, authz, cookie string) int {
		r := httptest.NewRequest("GET", "/api/machines", nil)
		r.RemoteAddr = "192.0.2.7:4242"
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
		}
		rec := httptest.NewRecorder()
		h(rec, r)
		return rec.Code
	}
	session, err := authStore.NewSession()
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		h             http.HandlerFunc
		authz, cookie string
		want          int
	}{
		"token":                 {a.requireAuth(ok), "Bearer " + secret, "", http.StatusNoContent},
		"session":               {a.requireAuth(ok), "", session, http.StatusNoContent},
		"nothing":               {a.requireAuth(ok), "", "", http.StatusUnauthorized},
		"wrong token":           {a.requireAuth(ok), "Bearer janus_" + tok.ID + "_nope", "", http.StatusUnauthorized},
		"token, session-only":   {a.requireSession(ok), "Bearer " + secret, "", http.StatusUnauthorized},
		"session, session-only": {a.requireSession(ok), "", session, http.StatusNoContent},
	} {
		if got := do(tc.h, tc.authz, tc.cookie); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}

	// A bad token counts against the address, like a bad password.
	for i := 0; i < 10; i++ {
		do(a.requireAuth(ok), "Bearer janus_x_y", "")
	}
	if got := do(a.requireAuth(ok), "Bearer "+secret, ""); got != http.StatusTooManyRequests {
		t.Errorf("after many wrong tokens: %d, want 429", got)
	}

	// Revoked: refused at once.
	if err := tokens.Revoke(tok.ID); err != nil {
		t.Fatal(err)
	}
	a.loginLimiter = auth.NewLoginLimiter()
	if got := do(a.requireAuth(ok), "Bearer "+secret, ""); got != http.StatusUnauthorized {
		t.Errorf("revoked token: %d", got)
	}
}
