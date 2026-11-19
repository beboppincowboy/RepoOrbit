package githubapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReleasesAndIssueEventsPages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("missing token")
		}
		if strings.HasSuffix(r.URL.Path, "/releases") {
			w.Header().Set("Link", `</repositories/1183888342/releases?per_page=100&page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[{"id":1,"author":null,"name":null,"body":null,"tag_name":"v1","html_url":"url","created_at":"2026-01-01T00:00:00Z","published_at":"2026-01-02T00:00:00Z","draft":false,"prerelease":true}]`))
		} else {
			_, _ = w.Write([]byte(`[{"id":2,"actor":null,"event":"labeled","created_at":"2026-01-01T00:00:00Z","url":"url","issue":{"id":3,"number":4},"label":{"name":"bug"}}]`))
		}
	}))
	defer server.Close()
	c := New("", "", "", server.URL, "")
	releases, next, err := c.FetchReleasesCursor(context.Background(), "token", "a/b", "")
	if err != nil || len(releases) != 1 || !releases[0].Prerelease || releases[0].Author != nil || next != "per_page=100&page=2" {
		t.Fatalf("releases=%+v next=%q err=%v", releases, next, err)
	}
	events, _, err := c.FetchIssueEventsCursor(context.Background(), "token", "a/b", "")
	if err != nil || len(events) != 1 || events[0].Issue.Number != 4 || events[0].Label.Name != "bug" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestU4cRejectsIncompleteAndUnsafePages(t *testing.T) {
	for _, tc := range []struct{ path, body, link string }{
		{"releases", `[{"id":1,"tag_name":"v","html_url":"url","created_at":"2026-01-01T00:00:00Z","published_at":"2026-01-01T00:00:00Z","draft":false}]`, ""},
		{"issues/events", `[{"id":1,"event":"closed","url":"url","created_at":"2026-01-01T00:00:00Z"}]`, ""},
		{"releases", `[]`, `<https://attacker.invalid/repos/a/b/releases?page=2>; rel="next"`},
		{"issues/events", `[]`, `</repos/a/b/issues?page=2>; rel="next"`},
		{"releases", `null`, ""},
	} {
		t.Run(tc.path+tc.body+tc.link, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Link", tc.link)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := New("", "", "", server.URL, "")
			var err error
			if tc.path == "releases" {
				_, _, err = c.FetchReleasesCursor(context.Background(), "token", "a/b", "")
			} else {
				_, _, err = c.FetchIssueEventsCursor(context.Background(), "token", "a/b", "")
			}
			if err == nil {
				t.Fatal("accepted incomplete or unsafe page")
			}
		})
	}
}
