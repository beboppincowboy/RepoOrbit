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

	// Test seams; nil means the real RequestMetadataRefresh, accessfence.InvalidateAccess and
	// accessfence.InvalidateInstallationGrant.
	refresh func(ctx context.Context, tx pgx.Tx, repoID int64) (bool, error)
	bump    func(ctx context.Context, tx pgx.Tx, scope accessfence.Scope, subjectID int64) error
	grant   func(ctx context.Context, tx pgx.Tx, installationID int64) error
}

// signalsActivity reports whether a delivery is a reconciliation signal for activity streams. Payload content is
// never stored; only a signed deletion adds a tombstone (see applyDeletion).
func signalsActivity(event string) bool {
	switch event {
	case "issues", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment", "release", "workflow_run", "workflow_job", "deployment_status":
		return true
	}
	return false
}

type webhookPayload struct {
	Action     string `json:"action"`
	Repository struct {
		ID int64 `json:"id"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Issue struct {
		ID     int64 `json:"id"`
		Number int64 `json:"number"`
	} `json:"issue"`
	Comment struct {
		ID int64 `json:"id"`
	} `json:"comment"`
	Release struct {
		ID int64 `json:"id"`
	} `json:"release"`
	Sender struct {
		ID int64 `json:"id"`
	} `json:"sender"`
	RepositoriesRemoved []json.RawMessage `json:"repositories_removed"` // installation_repositories only
}

// onlyGrantsAccess reports an installation or installation_repositories delivery that cannot end anyone's access:
// installation created or unsuspended, or repositories added with none removed. Any other action, including unknown
// ones, may revoke (fail closed).
func onlyGrantsAccess(event string, p webhookPayload) bool {
	switch event {
	case "installation":
		return p.Action == "created" || p.Action == "unsuspend"
	case "installation_repositories":
		return p.Action == "added" && len(p.RepositoriesRemoved) == 0
	}
	return false
}

// WebhookHandler serves POST /webhooks/github. Payloads are reconciliation signals only: the handler reads
// ids from them and schedules a refresh or bumps the access fence. It never writes metadata or grants access.
//
// Supported events: push, issues, pull_request, repository (edited, renamed, publicized, archived, unarchived)
// schedule a metadata refresh for an already selected repository; repository (privatized, deleted, transferred),
// installation and installation_repositories bump the access fence (an installation event that can only grant access
// records no fallback draft-wipe targets, see onlyGrantsAccess); github_app_authorization bumps that user's fence and also
// deletes their sessions and clears their stored tokens in the same transaction. Anything else that is validly signed is acknowledged without work.
func WebhookHandler(d WebhookDeps) http.Handler {
	if d.refresh == nil {
		d.refresh = RequestMetadataRefresh
	}
	if d.bump == nil {
		d.bump = accessfence.InvalidateAccess
	}
	if d.grant == nil {
		d.grant = accessfence.InvalidateInstallationGrant
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
	whActivity           // activity reconciliation signal only: no metadata refresh
)

func classifyWebhook(event, action string) whKind {
	switch event {
	case "push", "issues", "pull_request":
		return whOrdinary
	case "issue_comment", "pull_request_review", "pull_request_review_comment", "release", "workflow_run", "workflow_job", "deployment_status":
		return whActivity
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
	return db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		n, err := q.InsertWebhookDelivery(ctx, db.InsertWebhookDeliveryParams{DeliveryID: id, Event: event, ReceivedAt: pgtype.Timestamptz{Time: now, Valid: true}})
		if err != nil {
			return err
		}
		if n == 0 { // duplicate: already accepted, no second work
			return nil
		}
		switch {
		case k == whOrdinary || k == whActivity:
			if p.Repository.ID <= 0 {
				return errWebhookPayload{}
			}
			if k == whOrdinary {
				_, err = d.refresh(ctx, tx, p.Repository.ID)
			}
			if err == nil && (signalsActivity(event) || event == "repository") {
				// Job rows first (SignalActivity), then checkpoint rows (applyDeletion): the order sync commits take them in.
				if _, err = SignalActivity(ctx, tx, p.Repository.ID, now); err == nil {
					err = applyDeletion(ctx, tx, p.Repository.ID, event, p.Action, p)
				}
			}
		case event == "repository":
			if p.Repository.ID <= 0 {
				return errWebhookPayload{}
			}
			// Tighten visibility before scheduling cleanup when GitHub reports privatization.
			if p.Action == "privatized" {
				if err = q.MarkRepositoryPrivate(ctx, p.Repository.ID); err != nil {
					return err
				}
			}
			// Both take effect at commit, so their order here is only lock order: the repository's job row first, then the
			// fence rows, the same order a refresh commit takes them in (claim check, then StillCurrent). The refresh is a
			// no-op when the repository is unselected.
			if _, err = d.refresh(ctx, tx, p.Repository.ID); err == nil {
				err = d.bump(ctx, tx, accessfence.ScopeRepository, p.Repository.ID)
			}
		case event == "github_app_authorization":
			var uid int64
			uid, err = q.GetUserIDByGithubID(ctx, p.Sender.ID)
			if errors.Is(err, pgx.ErrNoRows) {
				err = nil // no such user here: nothing to invalidate
			} else if err == nil {
				// Bump first (cached reads stop), then end the credential: jobs find no token and pause without spending
				// attempts. Same order and queries as auth.Service.Revoke, in this delivery's transaction.
				if err = d.bump(ctx, tx, accessfence.ScopeUser, uid); err == nil {
					if err = q.DeleteUserSessions(ctx, uid); err == nil {
						err = q.ClearUserTokens(ctx, uid)
					}
				}
			}
		default: // installation, installation_repositories
			if p.Installation.ID <= 0 {
				return errWebhookPayload{}
			}
			if onlyGrantsAccess(event, p) {
				err = d.grant(ctx, tx, p.Installation.ID)
			} else {
				err = d.bump(ctx, tx, accessfence.ScopeInstallation, p.Installation.ID)
			}
		}
		return err
	})
}

func validWebhookSignature(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	digest, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(digest)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}
