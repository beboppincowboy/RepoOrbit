package issues

import (
	"net/http"

	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/issues/application"
	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

var statusLabels = domain.StatusLabels

func canChangeStatusLabels(r githubapp.Repo) bool {
	return domain.CanChangeStatusLabels(domainRepo(r))
}

// enableStatusLabels answers POST /api/repositories/{id}/status-labels (Idempotency-Key, body {}): it parses the
// request, runs application.EnableStatusLabels and maps its result. The operation creates the missing status labels
// with the actor's token, reuses existing ones, and turns status labels on once all three are confirmed on GitHub.
func (s *Service) enableStatusLabels(w http.ResponseWriter, r *http.Request) {
	sw := newSubmitWriter(w)
	var body struct{}
	if err := decodeStrict(sw, r, &body); err != nil {
		http.Error(sw, err.Error(), http.StatusBadRequest)
		return
	}
	userID, ok := auth.SignedInUserID(r.Context())
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(sw, "not found", http.StatusNotFound)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !domain.ValidKey(key) {
		http.Error(sw, idempotencyKeyRequired, http.StatusBadRequest)
		return
	}
	s.writeResult(w, s.commands().EnableStatusLabels(r.Context(), application.EnableStatusLabelsInput{UserID: userID, RepoID: repoID, Key: key}))
}

// disableStatusLabels answers DELETE /api/repositories/{id}/status-labels with 204: it parses the request, runs
// application.DisableStatusLabels and maps its result. Nothing is written to GitHub: the labels stay on the
// repository and its issues, and the Board shows "No status source" again.
func (s *Service) disableStatusLabels(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.SignedInUserID(r.Context())
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.writeResult(w, s.commands().DisableStatusLabels(r.Context(), userID, repoID))
}
