package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type failingDB struct{}

func (failingDB) Ping(context.Context) error { return errors.New("secret connection string") }

func TestHealthDoesNotLeakDatabaseErrors(t *testing.T) {
	h := Health(failingDB{})
	for _, tc := range []struct {
		path   string
		status int
	}{{"/live", 200}, {"/ready", 503}} {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if r.Code != tc.status || strings.Contains(r.Body.String(), "secret") {
			t.Fatalf("%s: %d %s", tc.path, r.Code, r.Body.String())
		}
	}
}
