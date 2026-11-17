// Package accessfence is the durable authorization fence (U3b).
//
// CONTRACT STUB: signatures frozen by docs/plans/2026-11-16-u3-contracts.md.
// U3b replaces the bodies; U3c calls Bump from webhooks; other units must not edit this package.
package accessfence

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type Scope string

const (
	ScopeUser         Scope = "user"         // GitHub authorization revoked / signed out of GitHub
	ScopeInstallation Scope = "installation" // installation deleted/suspended, repos removed
	ScopeRepository   Scope = "repository"   // repository deleted/privatized/transferred
)

var ErrNotImplemented = errors.New("accessfence: not implemented (U3b)")

// Bump durably invalidates access for a subject by incrementing its epoch, inside the
// caller's transaction (before any slower cleanup). Never grants access.
func Bump(ctx context.Context, tx pgx.Tx, scope Scope, subjectID int64) error {
	return ErrNotImplemented
}

// Snapshot captures epochs for a (user, repository) read before a remote fetch.
type Snapshot struct{ epochs map[Scope]map[int64]int64 }

// Take records current epochs for userID, repoID and every installation the user holds.
func Take(ctx context.Context, q pgx.Tx, userID, repoID int64) (Snapshot, error) {
	return Snapshot{}, ErrNotImplemented
}

// Valid reports, inside the commit transaction, that no tracked epoch moved since Take.
// Private commits and cached private reads must check it; false means withhold the update.
func (s Snapshot) Valid(ctx context.Context, tx pgx.Tx) (bool, error) {
	return false, ErrNotImplemented
}
