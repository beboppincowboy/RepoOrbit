// Package auth owns sign-in with GitHub, sessions, CSRF protection, and stored user tokens.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"golang.org/x/oauth2"
)

const (
	SessionLifetime = 7 * 24 * time.Hour // fixed; use never extends it
	stateLifetime   = 10 * time.Minute
	refreshSkew     = time.Minute
)

var ErrUnauthenticated = errors.New("not signed in")

// Viewer is the signed-in person for a request. Demo viewers have no stored user or tokens.
type Viewer struct {
	UserID int64
	Login  string
	Demo   bool
}

var DemoViewer = Viewer{Login: "demo-user", Demo: true}

type Service struct {
	Pool    *pgxpool.Pool
	Queries *db.Queries
	GitHub  *githubapp.Client
	Key     []byte
	Now     func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// timestamptz converts t to a database timestamp; the zero time becomes NULL.
func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

// BeginLogin stores a single-use state with its PKCE verifier and returns the GitHub URL plus the state
// that must also be set in the browser's cookie.
func (s *Service) BeginLogin(ctx context.Context) (authURL, state string, err error) {
	if state, err = randomToken(); err != nil {
		return "", "", err
	}
	// Opportunistic cleanup: nothing else removes abandoned states or expired sessions.
	if err = s.Queries.DeleteExpiredAuthRows(ctx, timestamptz(s.now())); err != nil {
		return "", "", err
	}
	verifier := oauth2.GenerateVerifier()
	if err = s.Queries.CreateOAuthState(ctx, db.CreateOAuthStateParams{StateHash: sha256Digest(state), CodeVerifier: verifier, ExpiresAt: timestamptz(s.now().Add(stateLifetime))}); err != nil {
		return "", "", err
	}
	return s.GitHub.AuthURL(state, verifier), state, nil
}

// CompleteLogin validates the callback and returns a new session token. The state must match the
// browser's cookie and be unused and unexpired; it is consumed even if the exchange then fails.
func (s *Service) CompleteLogin(ctx context.Context, queryState, cookieState, code string) (string, error) {
	if queryState == "" || code == "" || subtle.ConstantTimeCompare([]byte(queryState), []byte(cookieState)) != 1 {
		return "", errors.New("state mismatch")
	}
	verifier, err := s.Queries.ConsumeOAuthState(ctx, db.ConsumeOAuthStateParams{StateHash: sha256Digest(queryState), Now: timestamptz(s.now())})
	if err != nil {
		return "", errors.New("unknown or reused state")
	}
	tokens, err := s.GitHub.Exchange(ctx, code, verifier)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	gh, err := s.GitHub.FetchUser(ctx, tokens.AccessToken)
	if err != nil {
		return "", err
	}
	access, err := seal(s.Key, tokens.AccessToken)
	if err != nil {
		return "", err
	}
	var refresh []byte
	if tokens.RefreshToken != "" {
		if refresh, err = seal(s.Key, tokens.RefreshToken); err != nil {
			return "", err
		}
	}
	userID, err := s.Queries.UpsertUser(ctx, db.UpsertUserParams{GithubID: gh.ID, Login: gh.Login, AccessTokenEnc: access, RefreshTokenEnc: refresh, AccessExpiresAt: timestamptz(tokens.AccessExpiresAt), RefreshExpiresAt: timestamptz(tokens.RefreshExpiresAt)})
	if err != nil {
		return "", err
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	if err = s.Queries.CreateSession(ctx, db.CreateSessionParams{TokenHash: sha256Digest(token), UserID: userID, CreatedAt: timestamptz(now), ExpiresAt: timestamptz(now.Add(SessionLifetime))}); err != nil {
		return "", err
	}
	return token, nil
}

// ViewerForSession resolves a session token (the cookie value, not a GitHub token). Expired, revoked, and unknown tokens are all ErrUnauthenticated.
func (s *Service) ViewerForSession(ctx context.Context, sessionToken string) (Viewer, error) {
	if sessionToken == "" {
		return Viewer{}, ErrUnauthenticated
	}
	row, err := s.Queries.GetSession(ctx, db.GetSessionParams{TokenHash: sha256Digest(sessionToken), Now: timestamptz(s.now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return Viewer{}, ErrUnauthenticated
	}
	if err != nil {
		return Viewer{}, err
	}
	return Viewer{UserID: row.UserID, Login: row.Login}, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	return s.Queries.DeleteSession(ctx, sha256Digest(token))
}

func (s *Service) CSRFToken(sessionToken string) string { return csrfToken(s.Key, sessionToken) }

// AccessToken returns a usable GitHub user token, refreshing it when it is about to expire.
// The user row stays locked for the whole refresh, so concurrent callers wait and then reuse the
// new token instead of spending the (single-use, rotated) refresh token twice.
// ponytail: holds a row lock across one HTTP call; fine at local scale, move to an advisory lock if it contends.
func (s *Service) AccessToken(ctx context.Context, userID int64) (string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	row, err := q.LockUserTokens(ctx, userID)
	if err != nil {
		return "", err
	}
	if len(row.AccessTokenEnc) == 0 {
		return "", ErrUnauthenticated // revoked
	}
	access, err := open(s.Key, row.AccessTokenEnc)
	if err != nil {
		return "", err
	}
	if !row.AccessExpiresAt.Valid || row.AccessExpiresAt.Time.After(s.now().Add(refreshSkew)) {
		return access, nil
	}
	if len(row.RefreshTokenEnc) == 0 {
		return "", ErrUnauthenticated
	}
	refresh, err := open(s.Key, row.RefreshTokenEnc)
	if err != nil {
		return "", err
	}
	tokens, err := s.GitHub.Refresh(ctx, refresh)
	if err != nil {
		return "", fmt.Errorf("refresh: %w", err)
	}
	newAccess, err := seal(s.Key, tokens.AccessToken)
	if err != nil {
		return "", err
	}
	newRefresh, err := seal(s.Key, tokens.RefreshToken)
	if err != nil {
		return "", err
	}
	if err = q.UpdateUserTokens(ctx, db.UpdateUserTokensParams{ID: userID, AccessTokenEnc: newAccess, RefreshTokenEnc: newRefresh, AccessExpiresAt: timestamptz(tokens.AccessExpiresAt), RefreshExpiresAt: timestamptz(tokens.RefreshExpiresAt)}); err != nil {
		return "", err
	}
	return tokens.AccessToken, tx.Commit(ctx)
}

// Revoke ends every session of a user and wipes their stored GitHub tokens. Call it when GitHub
// reports the user's token is no longer valid.
func (s *Service) Revoke(ctx context.Context, userID int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	if err = accessfence.InvalidateAccess(ctx, tx, accessfence.ScopeUser, userID); err != nil { // before cleanup: cached reads must stop first
		return err
	}
	if err = q.DeleteUserSessions(ctx, userID); err != nil {
		return err
	}
	if err = q.ClearUserTokens(ctx, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
