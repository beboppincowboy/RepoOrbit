// Package ingestion owns repository synchronization.
//
// CONTRACT STUB: U3b replaces this file (delete it once implemented). U3c owns webhook*.go,
// U3d owns reconcile*.go and status*.go. Signatures frozen by docs/plans/2026-11-16-u3-contracts.md.
package ingestion

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

var ErrNotImplemented = errors.New("ingestion: not implemented (U3b)")

// RequestRefresh enqueues a metadata refresh for repoID inside tx, only when the repository
// has at least one active, follow-eligible selection (ordinary events never import
// unselected repositories). Used by selection changes (U3b), webhooks (U3c), polls (U3d).
// Returns whether work was scheduled.
func RequestRefresh(ctx context.Context, tx pgx.Tx, repoID int64) (bool, error) {
	return false, ErrNotImplemented
}
