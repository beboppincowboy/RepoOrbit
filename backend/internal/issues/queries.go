package issues

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

var (
	abandonedPendingOutcome         = json.RawMessage(`{"error":"abandoned before sending; nothing was sent to GitHub"}`)
	abandonedPendingRedactedOutcome = json.RawMessage(`{"error":"abandoned before sending; nothing was sent to GitHub","redacted":true}`)
)

func (s *Service) view(row db.GetWriteOperationRow) opView {
	state := row.State
	rawOutcome := row.Outcome
	if state == "pending" && row.CreatedAt.Valid && time.Since(row.CreatedAt.Time) > ingestion.StalePendingTimeout {
		state = "failed"
		if len(row.Draft) == 0 {
			rawOutcome = abandonedPendingRedactedOutcome
		} else {
			rawOutcome = abandonedPendingOutcome
		}
	}
	v := opView{ID: row.ID, RepositoryID: row.RepositoryID, Kind: row.Kind, State: state, Draft: row.Draft, Outcome: rawOutcome,
		Actor: row.ActorLogin, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
	if row.IssueNumber.Valid {
		v.IssueNumber = &row.IssueNumber.Int64
	}
	if row.Kind == "create" && state == "unknown" {
		q := "is:issue author:" + row.ActorLogin
		var d createDraft
		if json.Unmarshal(row.Draft, &d) == nil && d.Title != "" {
			q += ` in:title "` + strings.ReplaceAll(d.Title, `"`, "") + `"`
		}
		v.SearchURL = s.GitHubURL + "/" + row.RepositoryName + "/issues?q=" + url.QueryEscape(q)
	}
	return v
}

func (s *Service) capabilities(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, map[string]bool{"canCreateIssue": canCreate(d.Repo), "canCreateIssueWithMetadata": canCreateWithMetadata(d.Repo), "canManageIssues": canManage(d.Repo)})
}

// assignees lists the users GitHub would accept as assignees ({"assignees": [login]}), read with the viewer's own
// token, so editor suggestions pass the submit-time eligibility check. A viewer who cannot assign gets an empty list.
func (s *Service) assignees(w http.ResponseWriter, r *http.Request) {
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
	if !canManage(d.Repo) && !canCreateWithMetadata(d.Repo) {
		writeJSON(w, http.StatusOK, map[string][]string{"assignees": {}})
		return
	}
	token, ok := s.token(w, r.Context(), userID)
	if !ok {
		return
	}
	logins, err := s.GitHub.ListAssignees(r.Context(), token, d.Repo.FullName)
	if err != nil {
		s.upstream(w, r.Context(), userID, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"assignees": logins})
}

func (s *Service) list(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.SignedInUserID(r.Context())
	repoID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_, snap, ok := s.checkedAccess(w, r, userID, repoID)
	if !ok {
		return
	}
	rows, err := s.Queries.ListWriteOperations(r.Context(), db.ListWriteOperationsParams{UserID: userID, RepositoryID: repoID})
	if err != nil {
		s.internal(w, err)
		return
	}
	appRows, err := s.Queries.ListAppOperations(r.Context(), repoID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !s.fenceHolds(w, r.Context(), snap) {
		return
	}
	out := make([]opView, 0, len(rows)+len(appRows))
	for _, row := range rows {
		out = append(out, s.view(db.GetWriteOperationRow(row)))
	}
	for _, row := range appRows {
		draft, _ := json.Marshal(map[string]string{"label": row.Label})
		out = append(out, opView{ID: row.ID, RepositoryID: row.RepositoryID, Kind: "cleanup", State: row.State, IssueNumber: &row.IssueNumber,
			Draft: draft, Outcome: row.Outcome, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time})
	}
	slices.SortStableFunc(out, func(a, b opView) int { return b.CreatedAt.Compare(a.CreatedAt) })
	writeJSON(w, http.StatusOK, map[string]any{"operations": out})
}

func (s *Service) loadOperation(w http.ResponseWriter, r *http.Request) (int64, db.GetWriteOperationRow, repositories.Decision, accessfence.Snapshot, bool) {
	userID, ok := auth.SignedInUserID(r.Context())
	opID, idOK := pathID(r, "id")
	if !ok || !idOK {
		http.Error(w, "not found", http.StatusNotFound)
		return 0, db.GetWriteOperationRow{}, repositories.Decision{}, accessfence.Snapshot{}, false
	}
	load := func() (db.GetWriteOperationRow, bool) {
		row, err := s.Queries.GetWriteOperation(r.Context(), db.GetWriteOperationParams{ID: opID, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return row, false
		}
		if err != nil {
			s.internal(w, err)
			return row, false
		}
		return row, true
	}
	row, ok := load() // only to learn the repository; read again once access is known
	if !ok {
		return 0, row, repositories.Decision{}, accessfence.Snapshot{}, false
	}
	d, snap, ok := s.checkedAccess(w, r, userID, row.RepositoryID)
	if !ok {
		return 0, row, d, snap, false
	}
	if row, ok = load(); !ok || !s.fenceHolds(w, r.Context(), snap) {
		return 0, row, d, snap, false
	}
	return userID, row, d, snap, true
}

func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	if _, row, _, _, ok := s.loadOperation(w, r); ok {
		writeJSON(w, http.StatusOK, s.view(row))
	}
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
		// One at a time: a GitHub 401 must reach upstream unmasked by a canceled sibling check, so it revokes the token.
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
