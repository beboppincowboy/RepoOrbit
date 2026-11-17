package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDemoModeHandler(t *testing.T) {
	h := &Handler{Demo: true, AppURL: "http://app.test"}
	mux := http.NewServeMux()
	mux.Handle("/auth/", h.Routes())
	mux.Handle("/protected", h.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, ok := FromContext(r.Context())
		if !ok || !v.Demo {
			t.Errorf("viewer %+v %v", v, ok)
		}
	})))
	do := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	if w := do("GET", "/auth/me"); w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"demo"`) || !strings.Contains(w.Body.String(), `"csrfToken":null`) {
		t.Fatalf("me: %d %s", w.Code, w.Body)
	}
	// No sign-in routes exist in Demo mode, and a POST needs no cookie or CSRF token.
	if w := do("GET", "/auth/github/login"); w.Code != 404 {
		t.Fatalf("login in demo: %d", w.Code)
	}
	if w := do("POST", "/protected"); w.Code != 200 {
		t.Fatalf("protected in demo: %d", w.Code)
	}
}
