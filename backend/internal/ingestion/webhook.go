package ingestion

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
)

// maxWebhookBody bounds the request body. GitHub allows up to 25 MB, but every supported event is a small
// signal we re-verify against GitHub anyway; larger deliveries are refused and GitHub shows the failure.
const maxWebhookBody = 1 << 20

// WebhookDeps are the webhook handler's dependencies. An empty Secret rejects every delivery (fail closed).
type WebhookDeps struct {
	Pool   *pgxpool.Pool
	Secret []byte
	Clock  jobs.Clock // nil means wall time

	// Test seams; nil means the real RequestRefresh and accessfence.Bump.
	refresh func(ctx context.Context, tx pgx.Tx, repoID int64) (bool, error)
	bump    func(ctx context.Context, tx pgx.Tx, scope accessfence.Scope, subjectID int64) error
}

type webhookPayload struct {
	Action     string `json:"action"`
	Repository struct {
		ID int64 `json:"id"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender struct {
		ID int64 `json:"id"`
	} `json:"sender"`
}

// WebhookHandler serves POST /webhooks/github. Payloads are reconciliation signals only: the handler reads
// ids from them and schedules a refresh or bumps the access fence. It never writes metadata or grants access.
//
// Supported events: push, issues, pull_request, repository (edited, renamed, publicized, archived, unarchived)
// schedule a metadata refresh for an already selected repository; repository (privatized, deleted, transferred),
// installation, installation_repositories and github_app_authorization bump the access fence. Anything else
// that is validly signed is acknowledged without work.
func WebhookHandler(d WebhookDeps) http.Handler {
	if d.refresh == nil {
		d.refresh = RequestRefresh
	}
	if d.bump == nil {
		d.bump = accessfence.Bump
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !validWebhookSignature(d.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		id, event := r.Header.Get("X-GitHub-Delivery"), r.Header.Get("X-GitHub-Event")
		if id == "" || event == "" {
			http.Error(w, "missing delivery headers", http.StatusBadRequest)
			return
		}
		var p webhookPayload
		if json.Unmarshal(body, &p) != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		class := classifyWebhook(event, p.Action)
		if class == whUnsupported {
			w.WriteHeader(http.StatusOK) // acknowledged, no work, nothing stored
			return
		}
		if err := d.accept(r.Context(), id, event, class, p); err != nil {
			var bad errWebhookPayload
			if errors.As(err, &bad) {
				http.Error(w, "bad payload", http.StatusBadRequest)
				return
			}
			slog.Error("webhook not accepted", "event", event, "err", err)
			http.Error(w, "not accepted", http.StatusInternalServerError) // GitHub can redeliver
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

type errWebhookPayload struct{}

func (errWebhookPayload) Error() string { return "webhook payload lacks required id" }

type whKind int

const (
	whUnsupported whKind = iota
	whOrdinary           // refresh the repository if selected
	whSecurity           // bump the fence (and refresh if the event names a repository)
)

func classifyWebhook(event, action string) whKind {
	switch event {
	case "push", "issues", "pull_request":
		return whOrdinary
	case "repository":
		switch action {
		case "edited", "renamed", "publicized", "archived", "unarchived":
			return whOrdinary
		case "privatized", "deleted", "transferred":
			return whSecurity
		}
	case "installation", "installation_repositories", "github_app_authorization":
		return whSecurity
	}
	return whUnsupported
}

// accept stores the delivery and its work in one transaction; the caller acknowledges only on nil.
func (d WebhookDeps) accept(ctx context.Context, id, event string, k whKind, p webhookPayload) error {
	now := time.Now()
	if d.Clock != nil {
		now = d.Clock.Now()
	}
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	n, err := q.InsertWebhookDelivery(ctx, db.InsertWebhookDeliveryParams{DeliveryID: id, Event: event, ReceivedAt: pgtype.Timestamptz{Time: now, Valid: true}})
	if err != nil {
		return err
	}
	if n == 0 { // duplicate: already accepted, no second work
		return tx.Commit(ctx)
	}
	switch {
	case k == whOrdinary:
		if p.Repository.ID <= 0 {
			return errWebhookPayload{}
		}
		_, err = d.refresh(ctx, tx, p.Repository.ID)
	case event == "repository":
		if p.Repository.ID <= 0 {
			return errWebhookPayload{}
		}
		// Invalidate first, then reconcile the current state; the refresh is a no-op when unselected.
		if err = d.bump(ctx, tx, accessfence.ScopeRepository, p.Repository.ID); err == nil {
			_, err = d.refresh(ctx, tx, p.Repository.ID)
		}
	case event == "github_app_authorization":
		var uid int64
		uid, err = q.GetUserIDByGithubID(ctx, p.Sender.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil // no such user here: nothing to invalidate
		} else if err == nil {
			err = d.bump(ctx, tx, accessfence.ScopeUser, uid)
		}
	default: // installation, installation_repositories
		if p.Installation.ID <= 0 {
			return errWebhookPayload{}
		}
		err = d.bump(ctx, tx, accessfence.ScopeInstallation, p.Installation.ID)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validWebhookSignature(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}
