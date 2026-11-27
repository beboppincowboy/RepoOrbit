package ingestion

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var testSecret = []byte("s3cret")

func sign(secret []byte, body string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// post sends a delivery; an empty sig means "sign correctly with secret".
func post(h http.Handler, secret []byte, event, id, body, sig string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Delivery", id)
	if sig == "" {
		sig = sign(secret, body)
	}
	r.Header.Set("X-Hub-Signature-256", sig)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Nothing in these cases may touch the database: Pool is nil and would panic.
func TestWebhookRejectsWithoutWork(t *testing.T) {
	h := WebhookHandler(WebhookDeps{Secret: testSecret})
	zero := "sha256=" + strings.Repeat("0", 64)
	for name, tc := range map[string]struct {
		got  *httptest.ResponseRecorder
		want int
	}{
		"bad signature":    {post(h, testSecret, "push", "d1", `{}`, zero), 401},
		"wrong secret":     {post(h, []byte("other"), "push", "d1", `{}`, ""), 401},
		"malformed sig":    {post(h, testSecret, "push", "d1", `{}`, "nothex"), 401},
		"oversized":        {post(h, testSecret, "push", "d1", strings.Repeat("x", maxWebhookBody+1), ""), 413},
		"missing delivery": {post(h, testSecret, "push", "", `{}`, ""), 400},
		"unsupported":      {post(h, testSecret, "star", "d2", `{"x":1}`, ""), 200},
		"ping":             {post(h, testSecret, "ping", "d3", `{}`, ""), 200},
	} {
		if tc.got.Code != tc.want {
			t.Errorf("%s: status %d, want %d", name, tc.got.Code, tc.want)
		}
	}
}

func TestWebhookEmptySecretRejectsEverything(t *testing.T) {
	h := WebhookHandler(WebhookDeps{})
	if w := post(h, nil, "star", "d1", `{}`, ""); w.Code != 401 {
		t.Fatalf("status %d, want 401", w.Code)
	}
}

func TestWebhookMethod(t *testing.T) {
	h := WebhookHandler(WebhookDeps{Secret: testSecret})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webhooks/github", nil))
	if w.Code != 405 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestDeliveryWebhooksSignalActivity(t *testing.T) {
	for _, event := range []string{"workflow_run", "workflow_job", "deployment_status"} {
		if classifyWebhook(event, "completed") != whActivity || !signalsActivity(event) {
			t.Errorf("%s does not signal activity", event)
		}
	}
}
