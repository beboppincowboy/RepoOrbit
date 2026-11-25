package db

// SQL of generated queries, exported so callers can queue them in one pgx.Batch.
// Results scan in the column order of the matching *Row struct.
const (
	ListMetadataStatusesSQL   = listMetadataStatuses
	ListActivityStatusesSQL   = listActivityStatuses
	ListReviewStatusesSQL     = listReviewStatuses
	ListRepositoryArchivedSQL = listRepositoryArchived
)
