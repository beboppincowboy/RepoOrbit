package graph

import (
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

// Resolver serves the GraphQL queries. Demo selects which rows are visible: Demo mode sees only demo rows. In real mode every result is scoped
// to the signed-in user's selections and then filtered by a live access check (Access); demo rows never appear.
type Resolver struct {
	Queries    *db.Queries
	Demo       bool
	RepoAccess *repositories.Service // nil in Demo mode
}
