// Package application runs the Issue write use cases (create, update, recheck, status labels) in the order and with
// the transaction boundaries described in docs/api.md. It talks to the database, GitHub and the access checks only
// through the ports below, which package issues implements; it never imports net/http, pgx, githubapp, accessfence,
// ingestion or repositories (enforced by internal/archtest).
package application

import (
	"context"
	"errors"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

// Fence is an opaque access-fence snapshot taken by AccessChecker.CheckedAccess. The application only hands it back
// to the ports; it never inspects it.
type Fence any

// Repo is the repository as the live access check saw it.
type Repo struct {
	domain.Repo
	FullName string // "owner/name"
}

// AccessDecision is the verdict of an access check.
type AccessDecision uint8

const (
	AccessAllowed    AccessDecision = iota
	AccessNotFound                  // the repository is unknown or the user may not see it; the adapter wipes drafts for a private one
	AccessUnverified                // access could not be checked; RetryAt may say when to retry
	AccessMoved                     // the fence moved during the check twice in a row
)

// Access is the outcome of AccessChecker.CheckedAccess.
type Access struct {
	Decision AccessDecision
	Repo     Repo // meaningful only when Decision is AccessAllowed
	RetryAt  time.Time
	Fence    Fence // meaningful only when Decision is AccessAllowed
}

var (
	// ErrSignInRequired: the user has no usable GitHub token (answered 401).
	ErrSignInRequired = errors.New("sign in required")
	// ErrCredentialUnavailable: the token could not be loaded right now (answered 503).
	ErrCredentialUnavailable = errors.New("GitHub credential unavailable")
	// ErrAlreadyLinked: settling a create found its issue number already linked to another operation.
	ErrAlreadyLinked = errors.New("issue already linked to another operation")
)

// AccessChecker decides live repository access and supplies the user's own GitHub token.
type AccessChecker interface {
	// CheckedAccess takes a fence snapshot, runs a fresh access check, and retries once when the fence moved meanwhile.
	// An error is an internal failure.
	CheckedAccess(ctx context.Context, userID, repoID int64) (Access, error)
	// FenceCurrent reports whether the fence still holds, outside any transaction.
	FenceCurrent(ctx context.Context, fence Fence) (bool, error)
	// Token returns the user's GitHub token, or ErrSignInRequired / ErrCredentialUnavailable.
	Token(ctx context.Context, userID int64) (string, error)
	// Revoke drops the user's token after GitHub answered 401. Failures are logged, not returned.
	Revoke(ctx context.Context, userID int64)
}

// Issue is a GitHub issue as the write rules read it, plus the opaque payload settlement projects from.
type Issue struct {
	View         *domain.IssueView // number, URL, title, state, labels, assignees
	AuthorID     int64             // the author's GitHub id; 0 when GitHub sent no user
	Body         string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	PullRequest  bool // the number belongs to a pull request
	InRepository bool // the issue's repository URL matches the requested repository
	Payload      any  // the full GitHub issue; pass back unchanged in Settlement.Confirmed
}

// RemoteError is a failed GitHub call in domain terms. The gateway returns every call failure as *RemoteError
// wrapping the transport error, so errors.As still reaches the original.
type RemoteError struct {
	Kind   domain.ErrorKind
	Status int // the HTTP status GitHub answered; 0 for NotSent and OtherError
	Err    error
}

func (e *RemoteError) Error() string      { return e.Err.Error() }
func (e *RemoteError) Unwrap() error      { return e.Err }
func (e *RemoteError) Unauthorized() bool { return e.Kind == domain.Unauthorized }

// IssueGateway is the GitHub calls the write use cases make, all with the user's own token.
type IssueGateway interface {
	FetchIssue(ctx context.Context, token, fullName string, number int64) (Issue, error)
	// CreateIssue is sent once and never retried by the application.
	CreateIssue(ctx context.Context, token, fullName string, d domain.CreateDraft) (Issue, error)
	SetIssueState(ctx context.Context, token, fullName string, number int64, state string) error
	AddLabels(ctx context.Context, token, fullName string, number int64, names []string) error
	RemoveLabel(ctx context.Context, token, fullName string, number int64, name string) error
	AddAssignees(ctx context.Context, token, fullName string, number int64, logins []string) error
	RemoveAssignees(ctx context.Context, token, fullName string, number int64, logins []string) error
	IsAssignable(ctx context.Context, token, fullName, login string) (bool, error)
	CreateLabel(ctx context.Context, token, fullName, name, color string) error
	LabelExists(ctx context.Context, token, fullName, name string) (bool, error)
}

// Operation is a stored write operation with every field the use cases and the operation view read.
type Operation struct {
	ID                   int64
	RepositoryID         int64
	RepositoryName       string // "owner/name"
	Kind                 string // "create", "update", "status_labels" or "cleanup"
	State                string // "pending", "unknown", "succeeded" or "failed"
	IssueNumber          int64  // 0 when none
	ContentHash          []byte
	HasRequestedMetadata bool
	Draft                []byte    // nil once wiped
	Outcome              []byte    // raw stored outcome JSON
	SentAt               time.Time // zero when never sent
	StalePending         bool      // pending for over a minute (database clock)
	ActorLogin           string
	ActorGithubID        int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// PriorOperation is what replay needs from an operation found by idempotency key.
type PriorOperation struct {
	ID           int64
	RequestHash  []byte
	StalePending bool
}

// NewOperation is the row Reserve inserts as pending.
type NewOperation struct {
	UserID               int64
	RepositoryID         int64
	Key                  string
	Kind                 string
	IssueNumber          int64 // stored only for kind "update"
	RequestHash          []byte
	ContentHash          []byte
	HasRequestedMetadata bool
	Draft                []byte
}

// ReserveResult says how Reserve ended.
type ReserveResult uint8

const (
	Reserved                ReserveResult = iota // inserted as pending; the id is valid
	ReserveRace                                  // the key was inserted concurrently; replay again
	ReserveUnresolvedCreate                      // write_operations_unresolved_create: an earlier create of the same content is unresolved
	ReserveAccessMoved                           // the fence no longer held inside the insert transaction
)

// CreateKey identifies the creates an unresolved-create conflict is about.
type CreateKey struct {
	UserID       int64
	RepositoryID int64
	ContentHash  []byte
}

// Settle describes one settlement under the issue lock. Update and create-recheck pass LockNumber = the issue number;
// update leaves Link false, create-recheck sets it. A submit-time create passes LockNumber 0 and Link true: the store
// links the confirmed number, then locks it and projects with the plain projection.
type Settle struct {
	OperationID  int64
	RepositoryID int64
	Fence        Fence
	LockNumber   int64
	Link         bool
}

// Settlement is what the read callback found.
type Settlement struct {
	State     string // "succeeded", "failed" or "unknown"
	Outcome   domain.Outcome
	Confirmed any // Issue.Payload of the confirmed issue, or nil
}

// OperationStore is the write-operation persistence.
type OperationStore interface {
	FindByKey(ctx context.Context, userID int64, key string) (PriorOperation, bool, error)
	// Reserve checks the fence and inserts the operation as pending in one transaction (ON CONFLICT on the key does
	// nothing). The id is valid only for Reserved.
	Reserve(ctx context.Context, fence Fence, op NewOperation) (int64, ReserveResult, error)
	// FailStalePendingCreate fails stale pending creates matching k and returns how many it failed.
	FailStalePendingCreate(ctx context.Context, k CreateKey) (int64, error)
	// FindUnresolvedCreate and FindLatestCreate return the id of a matching create; found is false when none matches.
	FindUnresolvedCreate(ctx context.Context, k CreateKey) (id int64, found bool, err error)
	FindLatestCreate(ctx context.Context, k CreateKey) (id int64, found bool, err error)
	// MarkSent moves pending to unknown within the one-minute window (database clock). False means the claim was lost.
	MarkSent(ctx context.Context, id int64) (bool, error)
	FailStale(ctx context.Context, id int64) error
	// Scope returns an operation's repository and whether it is stale pending; found is false when the user has no
	// such operation.
	Scope(ctx context.Context, id, userID int64) (repoID int64, stalePending, found bool, err error)
	Get(ctx context.Context, id, userID int64) (Operation, bool, error)
	ActorGithubID(ctx context.Context, userID int64) (int64, error)
	// SettleUnderLock runs, in one transaction: lock_timeout 10s, take the issue lock (when LockNumber != 0), reset
	// lock_timeout, call read, check the fence (only for a confirmed, non-unknown result), finish the operation WHERE
	// state = 'unknown', and project the confirmed issue. A read error aborts the transaction and is returned as is.
	// A unique violation returns ErrAlreadyLinked.
	SettleUnderLock(ctx context.Context, s Settle, read func() (Settlement, error)) error
	// SettleStatusLabels finishes a status_labels operation without an issue lock: a success whose fence moved becomes
	// unknown with the "access changed before status labels could be turned on; recheck" error; status labels are
	// turned on only on success.
	SettleStatusLabels(ctx context.Context, opID, repoID int64, fence Fence, state string, out domain.Outcome) error
	// DisableStatusLabels checks the fence and turns status labels off in one transaction; moved reports a moved fence.
	DisableStatusLabels(ctx context.Context, repoID int64, fence Fence) (moved bool, err error)
}
