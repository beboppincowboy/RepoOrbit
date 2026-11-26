package domain

import "fmt"

type State string

const (
	Pending   State = "pending"
	Unknown   State = "unknown"
	Succeeded State = "succeeded"
	Failed    State = "failed"
)

func Transition(from, to State) error {
	if (from == "" && to == Pending) || (from == Pending && (to == Unknown || to == Failed)) || (from == Unknown && (to == Succeeded || to == Failed || to == Unknown)) {
		return nil
	}
	return fmt.Errorf("invalid transition from %q to %q", from, to)
}
