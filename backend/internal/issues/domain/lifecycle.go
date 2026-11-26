package domain

import "fmt"

// State is a write operation's state, as in the CHECK constraint of migrations/00015_write_operations.sql.
type State string

const (
	Pending   State = "pending"
	Unknown   State = "unknown"
	Succeeded State = "succeeded"
	Failed    State = "failed"
)

// Transition reports whether a write operation may move from one state to another ("" is a new operation). It is the
// domain model only: the database is the authority, through the conditional updates in queries/write_operations.sql
// (InsertWriteOperation, MarkWriteOperationSent, FinishWriteOperation, FailStalePendingCreate,
// FailStalePendingOperationByID). Change both together.
func Transition(from, to State) error {
	if (from == "" && to == Pending) || (from == Pending && (to == Unknown || to == Failed)) || (from == Unknown && (to == Succeeded || to == Failed || to == Unknown)) {
		return nil
	}
	return fmt.Errorf("invalid transition from %q to %q", from, to)
}
