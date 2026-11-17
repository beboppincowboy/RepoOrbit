package repositories

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

// Routes serves the browser-facing repository API. Mount it behind auth.Handler.Protect, which supplies the
// viewer and enforces Origin and CSRF checks on PUT and DELETE; the Go port and the Next proxy see identical rules.
func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/repositories/owned", s.owned)
	mux.HandleFunc("PUT /api/repositories/{id}/selection", s.selection(true))
	mux.HandleFunc("DELETE /api/repositories/{id}/selection", s.selection(false))
	return mux
}

func viewer(w http.ResponseWriter, r *http.Request) (int64, bool) {
	v, ok := auth.FromContext(r.Context())
	if !ok || v.Demo || v.UserID == 0 {
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return 0, false
	}
	return v.UserID, true
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

func (s *Service) selection(connect bool) http.HandlerFunc {
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
		if connect {
			err = s.Select(r.Context(), userID, id)
		} else {
			err = s.Deselect(r.Context(), userID, id)
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"selected": connect, "syncState": SyncState(connect)})
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
func (s *Service) Installed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	result := "failed"
	defer func() { http.Redirect(w, r, s.AppURL+"/repositories?install="+result, http.StatusFound) }()
	v, ok := auth.FromContext(r.Context())
	id, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if !ok || v.UserID == 0 || err != nil || id <= 0 {
		result = "rejected"
		return
	}
	token, err := s.Tokens.AccessToken(r.Context(), v.UserID)
	if err != nil {
		return
	}
	installs, complete, err := collect(func(p int) ([]githubapp.Installation, bool, error) {
		return s.GitHub.Installations(r.Context(), token, p)
	})
	for _, in := range installs {
		if in.ID == id {
			if err := s.Q.UpsertInstallation(r.Context(), db.UpsertInstallationParams{UserID: v.UserID, InstallationID: in.ID, AccountLogin: in.Account.Login}); err != nil {
				log.Printf("repositories: store installation: %v", err)
				return
			}
			s.invalidate(v.UserID)
			result = "ok"
			return
		}
	}
	if err == nil && complete {
		result = "rejected"
	}
}
