package graph

import (
	"context"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// userID is the signed-in user's id; real-mode resolvers never run without one.
func (r *Resolver) userID(ctx context.Context) (int64, error) {
	userID, ok := auth.SignedInUserID(ctx)
	if !ok {
		return 0, codedError(codeUnauthenticated, "sign in required")
	}
	return userID, nil
}

// Machine-readable GraphQL error codes (errors[].extensions.code). Clients branch on these, never on the message.
const (
	codeUnauthenticated  = "UNAUTHENTICATED"   // no real signed-in viewer
	codeAccessUnverified = "ACCESS_UNVERIFIED" // GitHub could not confirm access; extensions.retryAt (RFC 3339) may be set
)

func codedError(code, message string) error {
	return &gqlerror.Error{Message: message, Extensions: map[string]any{"code": code}}
}

// unverifiedError is the ACCESS_UNVERIFIED error; retryAt zero means GitHub gave no time.
func unverifiedError(retryAt time.Time) error {
	err := &gqlerror.Error{Message: "access could not be verified; retry later", Extensions: map[string]any{"code": codeAccessUnverified}}
	if !retryAt.IsZero() {
		err.Extensions["retryAt"] = retryAt.UTC().Format(time.RFC3339)
	}
	return err
}
