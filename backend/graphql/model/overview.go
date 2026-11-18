package model

import "github.com/repoorbit/repoorbit/backend/internal/analytics"

// RepositoryOverview pairs metrics with workspace metadata under the same access fence.
type RepositoryOverview struct {
	*analytics.Overview
	FullName  string
	SyncState string
	Activity  *ActivityStatus
}

// CrossRepositoryOverview adds the count of withheld selected repositories to the metrics, under the same access fence.
type CrossRepositoryOverview struct {
	*analytics.CrossOverview
	Withheld int
	RetryAt  *string
}
