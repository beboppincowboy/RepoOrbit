package model

import "github.com/repoorbit/repoorbit/backend/internal/analytics"

// RepositoryOverview pairs metrics with workspace metadata under the same access fence.
type RepositoryOverview struct {
	*analytics.Overview
	FullName  string
	SyncState string
	Activity  *ActivityStatus
}
