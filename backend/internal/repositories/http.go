package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

// Routes serves the browser-facing repository API. Mount it behind auth.Handler.Protect, which supplies the
// viewer and enforces Origin and CSRF checks on PUT and DELETE; the Go port and the Next proxy see identical rules.
func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/repositories/owned", s.owned)
	mux.HandleFunc("GET /api/repositories/starred", s.starred)
	mux.HandleFunc("PUT /api/repositories/{id}/selection", s.selection(true, s.Connect))
	mux.HandleFunc("PUT /api/repositories/{id}/follow", s.selection(true, s.Follow))
	mux.HandleFunc("DELETE /api/repositories/{id}/selection", s.selection(false, s.Deselect)) // also unfollows
	mux.HandleFunc("POST /api/repositories/{id}/activity/refresh", s.refreshActivity)
	return mux
}

// viewer returns the signed-in user's id, or writes 401 and returns ok=false (Demo viewers are refused).
func viewer(w http.ResponseWriter, r *http.Request) (int64, bool) {
	userID, ok := auth.SignedInUserID(r.Context())
	if !ok {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return 0, false
	}
	return userID, true
}

func (s *Service) owned(w http.ResponseWriter, r *http.Request) {
	userID, ok := viewer(w, r)
	if !ok {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	out, err := s.Owned(r.Context(), userID, page)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// starred serves the star snapshot. refresh=1 (page open, Refresh button) re-reads GitHub first; paging does not.
func (s *Service) starred(w http.ResponseWriter, r *http.Request) {
	userID, ok := viewer(w, r)
	if !ok {
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = min(max(page, 1), maxStarPages*githubPerPage/starredPerPage+1)
	out, err := s.Starred(r.Context(), userID, page, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Service) selection(connect bool, act func(ctx context.Context, userID, repoID int64) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := viewer(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err = act(r.Context(), userID, id); err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"selected": connect, "syncState": SyncStateFor(connect)})
	}
}

// fail maps service errors to statuses without leaking whether a repository exists.
func (s *Service) fail(w http.ResponseWriter, err error) {
	var unverified *UnverifiedError
	switch {
	case errors.Is(err, ErrSignedOut):
		http.Error(w, "sign in required", http.StatusUnauthorized)
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrNotOwned):
		http.Error(w, "only repositories on your personal account can be connected", http.StatusForbidden)
	case errors.As(err, &unverified):
		if !unverified.RetryAt.IsZero() {
			w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(unverified.RetryAt).Seconds())+1))
		}
		http.Error(w, "access could not be verified; retry later", http.StatusServiceUnavailable)
	default:
		log.Printf("repositories: %v", err)
		http.Error(w, "temporarily unavailable", http.StatusInternalServerError)
	}
}

// Installed handles GitHub's redirect after the app is installed. The installation_id is accepted only if it
// appears in the signed-in user's own installations, so a forged or foreign id is rejected. It always
// redirects to the configured app origin.
// Installed is an HTTP handler, not a query: it also writes the installation row.
func (s *Service) Installed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result := "failed"
	defer func() { http.Redirect(w, r, s.AppURL+"/repositories?install="+result, http.StatusFound) }()
	userID, ok := auth.SignedInUserID(r.Context())
	id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if !ok || err != nil || id <= 0 {
		result = "rejected"
		return
	}
	token, err := s.TokenSource.AccessToken(r.Context(), userID)
	if err != nil {
		return
	}
	installs, complete, err := collect(func(p int) ([]githubapp.Installation, bool, error) {
		return s.GitHub.FetchInstallations(r.Context(), token, p)
	})
	for _, in := range installs {
		if in.ID == id {
			if err := s.storeInstallation(r.Context(), userID, in); err != nil {
				log.Printf("repositories: store installation: %v", err)
				return
			}
			s.invalidate(userID)
			result = "ok"
			return
		}
	}
	if err == nil && complete {
		result = "rejected"
	}
}

// storeInstallation saves a connected installation and runs OnEligibilityChange in one transaction, so work paused
// for lack of access resumes with it.
func (s *Service) storeInstallation(ctx context.Context, userID int64, in githubapp.Installation) error {
	return db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := accessfence.BumpAccess(ctx, tx, accessfence.ScopeInstallation, in.ID); err != nil {
			return err
		}
		if err := s.Queries.WithTx(tx).UpsertInstallation(ctx, db.UpsertInstallationParams{UserID: userID, InstallationID: in.ID, AccountLogin: in.Account.Login}); err != nil {
			return err
		}
		if s.OnEligibilityChange != nil {
			return s.OnEligibilityChange(ctx, tx, userID)
		}
		return nil
	})
}

// refreshActivity queues an activity catch-up for a repository the viewer has selected. Selection and live
// access are both checked first; every denial is the same 404 so ids cannot be probed.
func (s *Service) refreshActivity(w http.ResponseWriter, r *http.Request) {
	userID, ok := viewer(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 || s.SignalActivity == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if sel, err := s.Queries.ListSelectedIDs(r.Context(), db.ListSelectedIDsParams{UserID: userID, RepositoryIds: []int64{id}}); err != nil {
		s.fail(w, err)
		return
	} else if len(sel) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if d := s.CheckOne(r.Context(), userID, id, true); d.Unverified {
		s.fail(w, &UnverifiedError{RetryAt: d.RetryAt})
		return
	} else if !d.Allowed {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	outcome, err := s.signalActivity(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusAccepted
	switch outcome {
	case "ineligible":
		http.Error(w, "not found", http.StatusNotFound)
		return
	case "dead":
		status = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"state": outcome})
}

func (s *Service) signalActivity(ctx context.Context, repoID int64) (string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	outcome, err := s.SignalActivity(ctx, tx, repoID)
	if err != nil {
		return "", err
	}
	return outcome, tx.Commit(ctx)
}
