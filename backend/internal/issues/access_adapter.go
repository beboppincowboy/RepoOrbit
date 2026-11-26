package issues

import (
	"context"
	"errors"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/issues/application"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

// accessAdapter implements application.AccessChecker on repositories.Service and accessfence. Its fence is an
// accessfence.Snapshot.
type accessAdapter struct{ s *Service }

// check is the live access check behind every issue route: an unknown repository is not found; a denial on a private
// repository wipes the actor's drafts there first.
func (a accessAdapter) check(ctx context.Context, userID, repoID int64, fresh bool) (repositories.Decision, application.AccessDecision, error) {
	private, err := a.s.Queries.GetRepositoryIsPrivate(ctx, repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return repositories.Decision{}, application.AccessNotFound, nil
	}
	if err != nil {
		return repositories.Decision{}, 0, err
	}
	d := a.s.Access.CheckOne(ctx, userID, repoID, fresh)
	switch {
	case d.Unverified:
		return d, application.AccessUnverified, nil
	case !d.Allowed:
		if private {
			if err := a.s.Queries.WipeActorDrafts(ctx, db.WipeActorDraftsParams{UserID: userID, RepositoryID: repoID}); err != nil {
				log.Printf("issues: wipe drafts: %v", err)
			}
		}
		return d, application.AccessNotFound, nil
	}
	return d, application.AccessAllowed, nil
}

// checked takes a fence snapshot, runs a fresh check and retries once when the fence moved meanwhile.
func (a accessAdapter) checked(ctx context.Context, userID, repoID int64) (repositories.Decision, application.AccessDecision, accessfence.Snapshot, error) {
	for attempt := 0; ; attempt++ {
		snap, err := accessfence.Take(ctx, a.s.Pool, userID, repoID)
		if err != nil {
			return repositories.Decision{}, 0, snap, err
		}
		d, decision, err := a.check(ctx, userID, repoID, true)
		if err != nil || decision != application.AccessAllowed {
			return d, decision, snap, err
		}
		current, err := snap.Current(ctx, a.s.Pool)
		switch {
		case err != nil:
			return d, 0, snap, err
		case current:
			return d, application.AccessAllowed, snap, nil
		case attempt == 1:
			return d, application.AccessMoved, snap, nil
		}
	}
}

func (a accessAdapter) CheckedAccess(ctx context.Context, userID, repoID int64) (application.Access, error) {
	d, decision, snap, err := a.checked(ctx, userID, repoID)
	if err != nil {
		return application.Access{}, err
	}
	return application.Access{Decision: decision, Repo: application.Repo{Repo: domainRepo(d.Repo), FullName: d.Repo.FullName}, RetryAt: d.RetryAt, Fence: snap}, nil
}

func (a accessAdapter) FenceCurrent(ctx context.Context, fence application.Fence) (bool, error) {
	return fence.(accessfence.Snapshot).Current(ctx, a.s.Pool)
}

func (a accessAdapter) Token(ctx context.Context, userID int64) (string, error) {
	token, err := a.s.Access.TokenSource.AccessToken(ctx, userID)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return "", application.ErrSignInRequired
	case err != nil:
		return "", application.ErrCredentialUnavailable
	}
	return token, nil
}

func (a accessAdapter) Revoke(ctx context.Context, userID int64) {
	if err := a.s.Access.TokenSource.Revoke(ctx, userID); err != nil {
		log.Printf("issues: revoke failed: %v", err)
	}
}
