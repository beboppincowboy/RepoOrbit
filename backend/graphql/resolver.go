package graph

import "github.com/repoorbit/repoorbit/backend/internal/db"

// Demo selects which rows are visible: Demo mode sees only demo rows, real mode never sees them.
// Per-user scoping of real rows arrives with selections.
type Resolver struct {
	Queries *db.Queries
	Demo    bool
}
