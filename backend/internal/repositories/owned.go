package repositories

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

const ownedPerPage = 30

type OwnedRepo struct {
	ID          int64  `json:"id,string"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Private     bool   `json:"private"`
	Selected    bool   `json:"selected"`
	SyncState   string `json:"syncState"`
}

// OwnedPage is one page of the user's personal-account repositories. When GitHub could not be reached the
// page falls back to the user's own selections (RefreshFailed) rather than showing an empty list.
type OwnedPage struct {
	Repositories  []OwnedRepo `json:"repositories"`
	NextPage      int         `json:"nextPage"` // 0 when there is no next page
	RefreshFailed bool        `json:"refreshFailed"`
	Withheld      int         `json:"withheld"` // selected private repositories that could not be verified
	RetryAt       *time.Time  `json:"retryAt"`
	InstallURL    string      `json:"installUrl"`
}

// ErrSignedOut means GitHub no longer accepts the user's token; their session has been ended.
var ErrSignedOut = errors.New("signed out")

func (s *Service) Owned(ctx context.Context, userID int64, page int) (OwnedPage, error) {
	token, err := s.Tokens.AccessToken(ctx, userID)
	if errors.Is(err, auth.ErrUnauthenticated) {
		return OwnedPage{}, ErrSignedOut
	}
	if err != nil {
		log.Printf("repositories: token unavailable: %v", err)
		return s.ownedFallback(ctx, userID, page, time.Time{})
	}
	repos, more, err := s.GitHub.OwnedRepos(ctx, token, page, ownedPerPage)
	var apiErr *githubapp.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Unauthorized():
		s.revoke(ctx, userID)
		return OwnedPage{}, ErrSignedOut
	case errors.As(err, &apiErr) && apiErr.RateLimited():
		return s.ownedFallback(ctx, userID, page, apiErr.RetryAt)
	case err != nil:
		return s.ownedFallback(ctx, userID, page, time.Time{})
	}
	ids := make([]int64, len(repos))
	for i, r := range repos {
		ids[i] = r.ID
	}
	selected, err := s.selectedSet(ctx, userID, ids)
	if err != nil {
		return OwnedPage{}, err
	}
	out := OwnedPage{Repositories: []OwnedRepo{}, InstallURL: s.InstallURL}
	for _, r := range repos {
		out.Repositories = append(out.Repositories, ownedRepo(r, selected[r.ID]))
	}
	if more {
		out.NextPage = page + 1
	}
	return out, nil
}

func ownedRepo(r githubapp.Repo, selected bool) OwnedRepo {
	return OwnedRepo{ID: r.ID, Name: r.FullName, Description: r.Description, Private: r.Private, Selected: selected, SyncState: SyncState(selected)}
}

func (s *Service) selectedSet(ctx context.Context, userID int64, ids []int64) (map[int64]bool, error) {
	rows, err := s.Q.ListSelectedIDs(ctx, db.ListSelectedIDsParams{UserID: userID, RepositoryIds: ids})
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(rows))
	for _, id := range rows {
		out[id] = true
	}
	return out, nil
}

// ownedFallback shows the user's own selections when GitHub cannot be refreshed. Stored public rows are the
// owner's own metadata and stay visible; private ones appear only if a check still verifies them.
func (s *Service) ownedFallback(ctx context.Context, userID int64, page int, retryAt time.Time) (OwnedPage, error) {
	out := OwnedPage{Repositories: []OwnedRepo{}, RefreshFailed: true, InstallURL: s.InstallURL}
	if !retryAt.IsZero() {
		out.RetryAt = &retryAt
	}
	if page > 1 {
		return out, nil // the snapshot is one page
	}
	rows, err := s.Q.ListSelectedRepositories(ctx, db.ListSelectedRepositoriesParams{UserID: userID, PageSize: 100})
	if err != nil {
		return OwnedPage{}, err
	}
	var privateIDs []int64
	for _, r := range rows {
		if r.IsPrivate {
			privateIDs = append(privateIDs, r.ID)
		}
	}
	checks := s.Check(ctx, userID, privateIDs, false)
	for _, r := range rows {
		if !r.IsPrivate || checks[r.ID].Allowed {
			out.Repositories = append(out.Repositories, OwnedRepo{ID: r.ID, Name: r.Name, Description: r.Description, Private: r.IsPrivate, Selected: true, SyncState: SyncState(true)})
		} else {
			out.Withheld++
			if out.RetryAt == nil && !checks[r.ID].RetryAt.IsZero() {
				t := checks[r.ID].RetryAt
				out.RetryAt = &t
			}
		}
	}
	return out, nil
}
