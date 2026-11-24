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
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

// statusLabels are a repository's status source (ADR 0003), in Board order, with the color each is created with.
// status_label_rank in SQL matches the same three names.
var statusLabels = []struct{ name, color string }{
	{"status:todo", "c5def5"},
	{"status:in-progress", "fbca04"},
	{"status:review", "0e8a16"},
}

// statusLabelsDraft is what turning status labels on asks for: the three labels, created where missing.
type statusLabelsDraft struct {
	Labels []string `json:"labels"`
}

// recheckGrace is how long after its send a status_labels operation with a missing label stays unknown on recheck.
// ponytail: a fixed bound on how long the sending request may still be creating labels; it times out well before.
const recheckGrace = 2 * time.Minute

// canChangeStatusLabels: status labels are a setting every viewer of the repository shares, so only Admin or Maintain
// users may turn them on or off, and only where issues can be written.
func canChangeStatusLabels(r githubapp.Repo) bool {
	return writable(r) && (r.Permissions.Admin || r.Permissions.Maintain)
}

// statusLabelState answers GET /api/repositories/{id}/status-labels: {enabled, canChange, labels?}. labels
// ([{name, exists}], read from GitHub with the viewer's token) is included only for a viewer who could turn status
// labels on and they are off, so the review step can say which labels will be created and which reused.
func (s *Service) statusLabelState(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.SignedInUserID(r.Context())
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	d, ok := s.access(w, r, userID, repoID, false)
	if !ok {
		return
	}
	enabled, err := s.Queries.GetStatusLabels(r.Context(), repoID)
	if err != nil {
		s.internal(w, err)
		return
	}
	type label struct {
		Name   string `json:"name"`
		Exists bool   `json:"exists"`
	}
	out := struct {
		Enabled   bool    `json:"enabled"`
		CanChange bool    `json:"canChange"`
		Labels    []label `json:"labels,omitempty"`
	}{Enabled: enabled, CanChange: canChangeStatusLabels(d.Repo)}
	if out.CanChange && !enabled {
		token, ok := s.token(w, r.Context(), userID)
		if !ok {
			return
		}
		for _, l := range statusLabels {
			exists, err := s.GitHub.LabelExists(r.Context(), token, d.Repo.FullName, l.name)
			if err != nil {
				s.upstream(w, r.Context(), userID, err)
				return
			}
			out.Labels = append(out.Labels, label{l.name, exists})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// enableStatusLabels answers POST /api/repositories/{id}/status-labels (Idempotency-Key, body {}): a reviewed
// operation that creates the missing status labels with the actor's token, reuses existing ones, and turns status
// labels on once all three are confirmed on GitHub.
func (s *Service) enableStatusLabels(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if err := decodeStrict(w, r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	d := &statusLabelsDraft{}
	for _, l := range statusLabels {
		d.Labels = append(d.Labels, l.name)
	}
	s.submit(w, r, command{kind: "status_labels", draft: d, allowed: canChangeStatusLabels})
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
		err := s.GitHub.CreateLabel(ctx, token, fullName, l.name, l.color)
		var api *githubapp.APIError
		if errors.As(err, &api) && api.Status == http.StatusUnprocessableEntity {
			err = nil // it already exists (or the name is refused); the read below decides
		} else if err == nil {
			out.Created = append(out.Created, l.name)
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

// readStatusLabels reads the three labels back into out.Unapplied ("create label X" for each missing one): succeeded
// when all exist, failed when any is missing, unknown when the read itself failed.
func (s *Service) readStatusLabels(ctx context.Context, token, fullName string, out outcome) (string, outcome) {
	out.Unapplied = nil
	for _, l := range statusLabels {
		exists, err := s.GitHub.LabelExists(ctx, token, fullName, l.name)
		if err != nil {
			_, msg := classify(err)
			out.Error = strings.TrimSpace(out.Error + " Reading the labels back failed: " + msg)
			return "unknown", out
		}
		if !exists {
			out.Unapplied = append(out.Unapplied, "create label "+l.name)
		}
	}
	return resolved(out), out
}

// finishStatusLabels records the outcome of an unknown status_labels operation and, when it succeeded, turns status
// labels on in the same transaction, unless the setting changed after the operation was recorded (a later Turn off
// stands). If access changed since snap (the snapshot the actor's Admin or Maintain check
// was made under) the setting is not changed and the operation stays unknown, so a recheck under current access
// finishes it.
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

// recheckStatusLabels resolves an unknown status_labels operation by reading the labels again: succeeded (and status
// labels turned on) when all three exist, failed when any is missing. Turning labels on again is always safe, so a
// missing label need not stay unknown, except within recheckGrace of the send, when the original request may still be
// creating labels: then it stays unknown (409). The actor must still be Admin or Maintain, since success changes a
// setting every viewer shares.
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
