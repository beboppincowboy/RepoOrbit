package issues

import (
	"context"
	"errors"
	"net/http"

	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

var statusLabels = domain.StatusLabels

type statusLabelsDraft struct {
	Labels []string `json:"labels"`
}

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
	moved, err := operationStore{s}.DisableStatusLabels(r.Context(), repoID, snap)
	switch {
	case err != nil:
		s.internal(w, err)
	case moved:
		http.Error(w, "access changed; retry", http.StatusServiceUnavailable)
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
	state, out := s.commands().ReadStatusLabels(ctx, token, fullName, out)
	if state == "failed" && !answered {
		state = "unknown"
	}
	return state, out
}

func (s *Service) finishStatusLabels(ctx context.Context, opID, repoID int64, snap accessfence.Snapshot, state string, out outcome) error {
	return operationStore{s}.SettleStatusLabels(ctx, opID, repoID, snap, state, out)
}
