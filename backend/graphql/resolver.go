package graph

import (
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

// Demo selects which rows are visible: Demo mode sees only demo rows. In real mode every result is scoped
// to the signed-in user's selections and then filtered by a live access check (Access); demo rows never appear.
type Resolver struct {
	Queries *db.Queries
	Demo    bool
	Access  *repositories.Service // nil in Demo mode
}
