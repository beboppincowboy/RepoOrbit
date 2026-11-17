package ingestion

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/repoorbit/repoorbit/backend/internal/db"
)

// credentialThrottledUntil is the time before which no job may use userID's credential, or the zero time when it is
// not rate limited. Reads the database; a nil q (no throttle store) and read errors mean "not throttled", since the
// GitHub response will rate limit the call again and defer it.
func credentialThrottledUntil(ctx context.Context, q *db.Queries, userID int64) time.Time {
	if q == nil {
		return time.Time{}
	}
	t, err := q.GetCredentialThrottle(ctx, userID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.Printf("ingestion: read credential throttle: %v", err)
		}
		return time.Time{}
	}
	return t.Time
}

// throttleCredential records that userID's credential is rate limited until `until` (never shortening an existing
// pause). Best effort: a lost write costs one extra GitHub call, which is rate limited and deferred again.
func throttleCredential(ctx context.Context, q *db.Queries, userID int64, until time.Time) {
	if q == nil {
		return
	}
	if err := q.SetCredentialThrottle(ctx, db.SetCredentialThrottleParams{UserID: userID, RetryAfter: pgtype.Timestamptz{Time: until, Valid: true}}); err != nil {
		log.Printf("ingestion: record credential throttle: %v", err)
	}
}
