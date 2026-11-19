package graph

import (
	"fmt"
	"strconv"
)

// Query limits, enforced here and in server.go; keep schema.graphqls comments in step.
const (
	maxPageSize        = 100  // largest allowed `first`
	maxRepositoryIDs   = 100  // largest allowed `repositoryIds` list
	maxQueryDepth      = 6    // nested selection levels
	maxQueryComplexity = 1200 // gqlgen complexity budget; fits the workspace overview (25 contributors, 50 timeline entries, 90 days)
)

// pageLimit validates a `first` argument: 1..maxPageSize.
func pageLimit(first int) (int, error) {
	if first < 1 || first > maxPageSize {
		return 0, fmt.Errorf("first must be between 1 and %d", maxPageSize)
	}
	return first, nil
}

// parsePositiveID parses a decimal id or cursor (both are database ids). A nil value means "none" and returns 0.
// It rejects anything that is not a positive integer.
func parsePositiveID(value *string) (int64, error) {
	if value == nil {
		return 0, nil
	}
	n, err := strconv.ParseInt(*value, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid cursor")
	}
	return n, nil
}
