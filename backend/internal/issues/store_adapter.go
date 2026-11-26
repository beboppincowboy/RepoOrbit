package issues

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/issues/application"
)

// operationStore implements application.OperationStore on the sqlc queries. Fences are accessfence.Snapshot values
// and confirmed issues are githubapp.Issue values.
type operationStore struct{ s *Service }

var errAccessMoved = errors.New("access changed")

func toOperation(row db.GetWriteOperationRow) application.Operation {
	return application.Operation{ID: row.ID, RepositoryID: row.RepositoryID, RepositoryName: row.RepositoryName, Kind: row.Kind, State: row.State,
		IssueNumber: row.IssueNumber.Int64, ContentHash: row.ContentHash, HasRequestedMetadata: row.HasRequestedMetadata, Draft: row.Draft,
		Outcome: row.Outcome, SentAt: row.SentAt.Time, StalePending: row.StalePending.Bool, ActorLogin: row.ActorLogin,
		ActorGithubID: row.ActorGithubID, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
}

func found[T any](v T, err error) (T, bool, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

func (o operationStore) FindByKey(ctx context.Context, userID int64, key string) (application.PriorOperation, bool, error) {
	row, ok, err := found(o.s.Queries.GetWriteOperationByKey(ctx, db.GetWriteOperationByKeyParams{UserID: userID, IdempotencyKey: key}))
	return application.PriorOperation{ID: row.ID, RequestHash: row.RequestHash, StalePending: row.StalePending.Bool}, ok, err
}

func (o operationStore) Reserve(ctx context.Context, fence application.Fence, op application.NewOperation) (int64, application.ReserveResult, error) {
	var id int64
	err := db.InTx(ctx, o.s.Pool, func(tx pgx.Tx) error {
		if current, err := fence.(accessfence.Snapshot).StillCurrent(ctx, tx); err != nil {
			return err
		} else if !current {
			return errAccessMoved
		}
		var err error
		id, err = o.s.Queries.WithTx(tx).InsertWriteOperation(ctx, db.InsertWriteOperationParams{UserID: op.UserID, RepositoryID: op.RepositoryID,
			IdempotencyKey: op.Key, Kind: op.Kind, IssueNumber: pgtype.Int8{Int64: op.IssueNumber, Valid: op.Kind == "update"}, RequestHash: op.RequestHash,
			ContentHash: op.ContentHash, HasRequestedMetadata: op.HasRequestedMetadata, Draft: op.Draft})
		return err
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, application.ReserveRace, nil
	case errors.Is(err, errAccessMoved):
		return 0, application.ReserveAccessMoved, nil
	case errors.As(err, &pgErr) && pgErr.ConstraintName == "write_operations_unresolved_create":
		return 0, application.ReserveUnresolvedCreate, nil
	case err != nil:
		return 0, 0, err
	}
	return id, application.Reserved, nil
}

func (o operationStore) FailStalePendingCreate(ctx context.Context, k application.CreateKey) (int64, error) {
	return o.s.Queries.FailStalePendingCreate(ctx, db.FailStalePendingCreateParams{UserID: k.UserID, RepositoryID: k.RepositoryID, ContentHash: k.ContentHash})
}

func (o operationStore) FindUnresolvedCreate(ctx context.Context, k application.CreateKey) (int64, bool, error) {
	return found(o.s.Queries.FindUnresolvedCreate(ctx, db.FindUnresolvedCreateParams{UserID: k.UserID, RepositoryID: k.RepositoryID, ContentHash: k.ContentHash}))
}

func (o operationStore) FindLatestCreate(ctx context.Context, k application.CreateKey) (int64, bool, error) {
	return found(o.s.Queries.FindLatestCreate(ctx, db.FindLatestCreateParams{UserID: k.UserID, RepositoryID: k.RepositoryID, ContentHash: k.ContentHash}))
}

func (o operationStore) MarkSent(ctx context.Context, id int64) (bool, error) {
	n, err := o.s.Queries.MarkWriteOperationSent(ctx, id)
	return n > 0, err
}

func (o operationStore) FailStale(ctx context.Context, id int64) error {
	return o.s.Queries.FailStalePendingOperationByID(ctx, id)
}

func (o operationStore) Scope(ctx context.Context, id, userID int64) (int64, bool, bool, error) {
	row, ok, err := found(o.s.Queries.GetWriteOperationScope(ctx, db.GetWriteOperationScopeParams{ID: id, UserID: userID}))
	return row.RepositoryID, row.StalePending.Bool, ok, err
}

func (o operationStore) Get(ctx context.Context, id, userID int64) (application.Operation, bool, error) {
	row, ok, err := found(o.s.Queries.GetWriteOperation(ctx, db.GetWriteOperationParams{ID: id, UserID: userID}))
	return toOperation(row), ok, err
}

func (o operationStore) ActorGithubID(ctx context.Context, userID int64) (int64, error) {
	return o.s.Queries.GetUserGithubID(ctx, userID)
}

func (o operationStore) SettleUnderLock(ctx context.Context, s application.Settle, read func() (application.Settlement, error)) error {
	snap := s.Fence.(accessfence.Snapshot)
	err := db.InTx(ctx, o.s.Pool, func(tx pgx.Tx) (err error) {
		if s.LockNumber != 0 {
			if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
				return err
			}
			if err = ingestion.LockIssue(ctx, tx, s.RepositoryID, s.LockNumber); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = 0`); err != nil {
				return err
			}
		}
		res, err := read()
		if err != nil {
			return err
		}
		body, err := json.Marshal(res.Outcome)
		if err != nil {
			return err
		}
		confirmed, isConfirmed := res.Confirmed.(githubapp.Issue)
		current := false
		if isConfirmed && res.State != "unknown" {
			if current, err = snap.StillCurrent(ctx, tx); err != nil {
				return err
			}
		}
		number := pgtype.Int8{}
		if s.Link && isConfirmed {
			number = pgtype.Int8{Int64: confirmed.Number, Valid: true}
		}
		n, err := o.s.Queries.WithTx(tx).FinishWriteOperation(ctx, db.FinishWriteOperationParams{ID: s.OperationID, State: res.State, Outcome: body, IssueNumber: number})
		if err != nil || n == 0 || !current {
			return err
		}
		if s.LockNumber != 0 {
			return ingestion.ProjectConfirmedIssue(ctx, tx, s.RepositoryID, confirmed)
		}
		if err = ingestion.LockIssue(ctx, tx, s.RepositoryID, confirmed.Number); err != nil {
			return err
		}
		return ingestion.ProjectIssue(ctx, tx, s.RepositoryID, confirmed)
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return application.ErrAlreadyLinked
	}
	return err
}

func (o operationStore) SettleStatusLabels(ctx context.Context, opID, repoID int64, fence application.Fence, state string, out outcome) error {
	snap := fence.(accessfence.Snapshot)
	return db.InTx(ctx, o.s.Pool, func(tx pgx.Tx) error {
		q := o.s.Queries.WithTx(tx)
		if state == "succeeded" {
			current, err := snap.StillCurrent(ctx, tx)
			if err != nil {
				return err
			}
			if !current {
				state, out.Error = "unknown", "access changed before status labels could be turned on; recheck"
			}
		}
		body, err := json.Marshal(out)
		if err != nil {
			return err
		}
		n, err := q.FinishWriteOperation(ctx, db.FinishWriteOperationParams{ID: opID, State: state, Outcome: body})
		if err != nil || n == 0 || state != "succeeded" {
			return err
		}
		_, err = q.EnableStatusLabels(ctx, db.EnableStatusLabelsParams{RepositoryID: repoID, OperationID: opID})
		return err
	})
}

func (o operationStore) DisableStatusLabels(ctx context.Context, repoID int64, fence application.Fence) (bool, error) {
	err := db.InTx(ctx, o.s.Pool, func(tx pgx.Tx) error {
		if current, err := fence.(accessfence.Snapshot).StillCurrent(ctx, tx); err != nil {
			return err
		} else if !current {
			return errAccessMoved
		}
		_, err := o.s.Queries.WithTx(tx).DisableStatusLabels(ctx, repoID)
		return err
	})
	if errors.Is(err, errAccessMoved) {
		return true, nil
	}
	return false, err
}
