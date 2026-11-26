package issues

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

var statusLabels = domain.StatusLabels

type statusLabelsDraft struct {
	Labels []string `json:"labels"`
}

const recheckGrace = 2 * time.Minute

func canChangeStatusLabels(r githubapp.Repo) bool {
	return domain.CanChangeStatusLabels(domainRepo(r))
}

// enableStatusLabels answers POST /api/repositories/{id}/status-labels (Idempotency-Key, body {}): a reviewed
// operation that creates the missing status labels with the actor's token, reuses existing ones, and turns status
// labels on once all three are confirmed on GitHub.
func (s *Service) enableStatusLabels(w http.ResponseWriter, r *http.Request) {
	sw := newSubmitWriter(w)
	var body struct{}
	if err := decodeStrict(sw, r, &body); err != nil {
		http.Error(sw, err.Error(), http.StatusBadRequest)
		return
	}
	d := &statusLabelsDraft{}
	for _, l := range statusLabels {
		d.Labels = append(d.Labels, l.Name)
	}
	s.submit(sw, r, command{kind: "status_labels", draft: d, allowed: canChangeStatusLabels})
}

// disableStatusLabels answers DELETE /api/repositories/{id}/status-labels with 204. Nothing is written to GitHub: the
// labels stay on the repository and its issues, and the Board shows "No status source" again.
func (s *Service) disableStatusLabels(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.SignedInUserID(r.Context())
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	d, snap, ok := s.checkedAccess(w, r, userID, repoID)
	if !ok {
		return
	}
	if !canChangeStatusLabels(d.Repo) {
		http.Error(w, "only Admin or Maintain users can turn status labels off", http.StatusForbidden)
		return
	}
	err := db.InTx(r.Context(), s.Pool, func(tx pgx.Tx) error {
		if current, err := snap.StillCurrent(r.Context(), tx); err != nil {
			return err
		} else if !current {
			return errAccessMoved
		}
		_, err := s.Queries.WithTx(tx).DisableStatusLabels(r.Context(), repoID)
		return err
	})
	switch {
	case errors.Is(err, errAccessMoved):
		http.Error(w, "access changed; retry", http.StatusServiceUnavailable)
	case err != nil:
		s.internal(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// ensureStatusLabels creates each status label, stopping at the first error, then reads all three back: the outcome is
// what the read shows. A label that already exists, in any case, is answered 422 and reused, so nothing here can
// duplicate anything; an ambiguous step only leaves the operation unknown when the read still misses a label.
func (s *Service) ensureStatusLabels(ctx context.Context, userID int64, token, fullName string) (string, outcome) {
	out := outcome{}
	answered := true
	for _, l := range statusLabels {
		err := s.GitHub.CreateLabel(ctx, token, fullName, l.Name, l.Color)
		var api *githubapp.APIError
		if errors.As(err, &api) && api.Status == http.StatusUnprocessableEntity {
			err = nil
		} else if err == nil {
			out.Created = append(out.Created, l.Name)
		}
		if err != nil {
			s.revokeIfUnauthorized(ctx, userID, err)
			var state string
			state, out.Error = classify(err)
			answered = state == "failed"
			break
		}
	}
	state, out := s.readStatusLabels(ctx, token, fullName, out)
	if state == "failed" && !answered {
		state = "unknown"
	}
	return state, out
}

func (s *Service) readStatusLabels(ctx context.Context, token, fullName string, out outcome) (string, outcome) {
	out.Unapplied = nil
	exists, err := mapConcurrent(ctx, statusLabels, func(ctx context.Context, l domain.StatusLabel) (bool, error) {
		return s.GitHub.LabelExists(ctx, token, fullName, l.Name)
	})
	if err != nil {
		_, msg := classify(err)
		out.Error = strings.TrimSpace(out.Error + " Reading the labels back failed: " + msg)
		return "unknown", out
	}
	for i, l := range statusLabels {
		if !exists[i] {
			out.Unapplied = append(out.Unapplied, "create label "+l.Name)
		}
	}
	return domain.Resolved(out), out
}

func (s *Service) finishStatusLabels(ctx context.Context, opID, repoID int64, snap accessfence.Snapshot, state string, out outcome) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := s.Queries.WithTx(tx)
		if state == "succeeded" {
			current, err := snap.StillCurrent(ctx, tx)
			if err != nil {
				return err
			}
			if !current {
				state, out.Error = "unknown", "access changed before status labels could be turned on; recheck"
			}
		}
		body, err := json.Marshal(out)
		if err != nil {
			return err
		}
		n, err := q.FinishWriteOperation(ctx, db.FinishWriteOperationParams{ID: opID, State: state, Outcome: body})
		if err != nil || n == 0 || state != "succeeded" {
			return err
		}
		_, err = q.EnableStatusLabels(ctx, db.EnableStatusLabelsParams{RepositoryID: repoID, OperationID: opID})
		return err
	})
}

func (s *Service) recheckStatusLabels(w http.ResponseWriter, r *http.Request, userID int64, row db.GetWriteOperationRow, d repositories.Decision, snap accessfence.Snapshot) {
	ctx := r.Context()
	if !canChangeStatusLabels(d.Repo) {
		http.Error(w, "your current GitHub permission does not allow this action", http.StatusForbidden)
		return
	}
	token, ok := s.token(w, ctx, userID)
	if !ok {
		return
	}
	var sent outcome
	_ = json.Unmarshal(row.Outcome, &sent)
	state, out := s.readStatusLabels(ctx, token, d.Repo.FullName, outcome{Created: sent.Created})
	if state == "unknown" {
		s.upstream(w, ctx, userID, errors.New("label read failed"))
		return
	}
	if state == "failed" && row.SentAt.Valid && time.Since(row.SentAt.Time) < recheckGrace {
		if s.fenceHolds(w, ctx, snap) {
			writeJSON(w, http.StatusConflict, s.view(row))
		}
		return
	}
	if err := s.finishStatusLabels(context.WithoutCancel(ctx), row.ID, row.RepositoryID, snap, state, out); err != nil {
		s.internal(w, err)
		return
	}
	s.respond(w, ctx, http.StatusOK, userID, row.ID, snap)
}
