package application

import "time"

// ResultKind is what a use case decided. The HTTP adapter maps each kind to today's status code and body.
type ResultKind uint8

const (
	ResultOperation             ResultKind = iota // Operation; 201 when Created (202 if its state is unknown), else 200
	ResultMismatch                                // recheck could not confirm: 409 with the unchanged Operation
	ResultNoContent                               // 204
	ResultNotFound                                // 404 "not found"
	ResultIssueNotFound                           // 404 "issue not found"
	ResultIssueNumberRequired                     // 400 "issueNumber is required to recheck a create"
	ResultAccessUnverified                        // 503 "access could not be verified; retry later", Retry-After from RetryAt
	ResultAccessCheckMoved                        // 503 "access changed while it was being checked; retry"
	ResultLoadAccessMoved                         // 503 "access changed while loading; retry"
	ResultSubmitAccessMoved                       // 503 "access changed while submitting; nothing was sent"
	ResultRecordedAccessMoved                     // 503 "access changed; the operation was recorded and may have been sent: retry with the same Idempotency-Key"
	ResultDisableAccessMoved                      // 503 "access changed; retry"
	ResultKeyReuse                                // 422 "idempotency key reused with a different request"
	ResultForbidden                               // 403 "your current GitHub permission does not allow this action"
	ResultStatusLabelsForbidden                   // 403 "only Admin or Maintain users can turn status labels off"
	ResultSignInRequired                          // 401 "sign in required"
	ResultCredentialUnavailable                   // 503 "GitHub credential unavailable; retry later"
	ResultUpstream                                // 503 "GitHub could not be reached; nothing was sent"
	ResultConflict                                // 409 {"error":"changed on GitHub","current":Current}
	ResultUnassignable                            // 422 "cannot be assigned in this repository: " + Logins joined by ", "
	ResultDuplicateCreate                         // 409 {"error":Message,"operationId":OperationID}
	ResultCreateChanged                           // 503 "an earlier create of this issue changed while submitting; nothing was sent, retry"
	ResultInternal                                // 500 "temporarily unavailable"; Err is logged
)

// Disposition is the RepoOrbit-Write-Disposition value a result carries. The HTTP adapter writes it on responses with
// status 400 or higher, and on a 500 only when it is DispositionPossiblyRecorded.
type Disposition string

const (
	DispositionNone             Disposition = ""
	DispositionNotRecorded      Disposition = "not-recorded"
	DispositionPossiblyRecorded Disposition = "possibly-recorded"
)

// Current is the live issue a stale expectation is reported against.
type Current struct {
	State     string
	Labels    []string
	Assignees []string
	Status    string // domain.BoardStatus
	UpdatedAt time.Time
}

// Result is a use case's typed answer.
type Result struct {
	Kind        ResultKind
	Disposition Disposition
	Operation   Operation // ResultOperation, ResultMismatch
	Created     bool      // ResultOperation: answer 201/202 instead of 200
	RetryAt     time.Time // ResultAccessUnverified; zero when unknown
	Current     Current   // ResultConflict
	Logins      []string  // ResultUnassignable
	Message     string    // ResultDuplicateCreate
	OperationID int64     // ResultDuplicateCreate
	Err         error     // ResultInternal
}
