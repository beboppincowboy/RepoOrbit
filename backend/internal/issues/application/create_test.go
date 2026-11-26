package application

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

func pusher() Access {
	a := triage()
	a.Repo.Permissions.Push = true
	return a
}

func createInput(d domain.CreateDraft) CreateIssueInput {
	return CreateIssueInput{UserID: 1, RepoID: 42, Key: "key-1", Draft: d}
}

func createHash(t *testing.T, d domain.CreateDraft) []byte {
	t.Helper()
	draft, err := json.Marshal(&d)
	if err != nil {
		t.Fatal(err)
	}
	return domain.RequestHash("create", 42, 0, draft)
}

func newCreateFakes() *fakes {
	f := newFakes()
	f.access = pusher()
	f.created = Issue{View: domain.NewIssueView(9, "https://github.test/o/r/issues/9", "T", "open", []string{"bug"}, nil), Payload: "created"}
	return f
}

func count(f *fakes, c string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, x := range f.calls {
		if x == c {
			n++
		}
	}
	return n
}

func id(n int64) *int64 { return &n }

var metaDraft = domain.CreateDraft{Title: "T", Body: "B", Labels: []string{"bug"}, Assignees: []string{"amy"}}

// The happy path records exactly what today's submit records, sends once, and settles with LockNumber 0 and Link true
// on the confirmed payload; the assignee GitHub did not apply is listed and fails the create.
func TestCreateSendsOnceAndSettlesLinked(t *testing.T) {
	f := newCreateFakes()
	res := f.commands().CreateIssue(context.Background(), createInput(metaDraft))
	want := []string{"Access.CheckedAccess", "Store.FindByKey", "Access.Token", "GitHub.IsAssignable", "Store.Reserve", "Store.MarkSent",
		"GitHub.CreateIssue", "Store.SettleUnderLock", "Store.Get", "Access.FenceCurrent"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	draft, _ := json.Marshal(&metaDraft)
	wantOp := NewOperation{UserID: 1, RepositoryID: 42, Key: "key-1", Kind: "create", RequestHash: createHash(t, metaDraft),
		ContentHash: domain.ContentHash("T", "B"), HasRequestedMetadata: true, Draft: draft}
	if !reflect.DeepEqual(f.newOps, []NewOperation{wantOp}) {
		t.Errorf("reserved %+v, want %+v", f.newOps, wantOp)
	}
	if f.settle != (Settle{OperationID: 11, RepositoryID: 42, Fence: "fence", LockNumber: 0, Link: true}) {
		t.Errorf("settle %+v", f.settle)
	}
	s := f.settled
	if s.Confirmed != "created" || s.State != "failed" || s.Outcome.Issue != f.created.View || !slices.Equal(s.Outcome.Unapplied, []string{"add assignee amy"}) {
		t.Errorf("settled %+v", s)
	}
	if res.Kind != ResultOperation || !res.Created || res.Disposition != DispositionPossiblyRecorded || res.Operation.ID != 11 {
		t.Errorf("got %+v", res)
	}
}

func TestCreateWithoutMetadataSucceeds(t *testing.T) {
	f := newCreateFakes()
	f.access = triage()
	f.access.Repo.Permissions = domain.RepoPermissions{Pull: true}
	res := f.commands().CreateIssue(context.Background(), createInput(domain.CreateDraft{Title: "T"}))
	if res.Kind != ResultOperation || !res.Created || f.settled.State != "succeeded" || f.newOps[0].HasRequestedMetadata || f.called("GitHub.IsAssignable") {
		t.Fatalf("got %+v settled %+v ops %+v", res, f.settled, f.newOps)
	}
}

// A failed or ambiguous create is sent exactly once and never retried; nothing is confirmed, and a 401 revokes.
func TestCreateSendsExactlyOnceOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		state  string
		msg    string
		revoke bool
	}{
		{"5xx", &RemoteError{Kind: domain.HTTPError, Status: 502, Err: errors.New("502")}, "unknown", "GitHub answered status 502; the change may or may not have been applied", false},
		{"transport", &RemoteError{Kind: domain.OtherError, Err: errors.New("reset")}, "unknown", "", false},
		{"bare error", errors.New("reset"), "unknown", "", false},
		{"422", &RemoteError{Kind: domain.HTTPError, Status: 422, Err: errors.New("422")}, "failed", "GitHub rejected the request (status 422)", false},
		{"401", &RemoteError{Kind: domain.Unauthorized, Status: 401, Err: errors.New("401")}, "failed", "GitHub rejected the request (status 401)", true},
	} {
		f := newCreateFakes()
		f.createErr = tc.err
		res := f.commands().CreateIssue(context.Background(), createInput(metaDraft))
		_, wantMsg := domain.Classify(domain.OtherError, 0)
		if tc.msg != "" {
			wantMsg = tc.msg
		}
		if n := count(f, "GitHub.CreateIssue"); n != 1 {
			t.Errorf("%s: CreateIssue called %d times", tc.name, n)
		}
		s := f.settled
		if s.State != tc.state || s.Confirmed != nil || s.Outcome.Issue != nil || s.Outcome.Error != wantMsg || len(s.Outcome.Unapplied) != 0 {
			t.Errorf("%s: settled %+v", tc.name, s)
		}
		if f.settle != (Settle{OperationID: 11, RepositoryID: 42, Fence: "fence", LockNumber: 0, Link: true}) {
			t.Errorf("%s: settle %+v", tc.name, f.settle)
		}
		if f.called("Access.Revoke") != tc.revoke {
			t.Errorf("%s: revoke %v, calls %v", tc.name, !tc.revoke, f.calls)
		}
		if res.Kind != ResultOperation || !res.Created || res.Operation.State != tc.state {
			t.Errorf("%s: got %+v", tc.name, res)
		}
	}
}

// Labels or assignees need CanCreateWithMetadata; there is no author fallback for a create.
func TestCreatePermission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		perms domain.RepoPermissions
		d     domain.CreateDraft
		ok    bool
	}{
		{"pull, plain", domain.RepoPermissions{Pull: true}, domain.CreateDraft{Title: "T"}, true},
		{"pull, labels", domain.RepoPermissions{Pull: true}, domain.CreateDraft{Title: "T", Labels: []string{"bug"}}, false},
		{"triage, assignees", domain.RepoPermissions{Pull: true, Triage: true}, domain.CreateDraft{Title: "T", Assignees: []string{"amy"}}, false},
		{"push, labels", domain.RepoPermissions{Pull: true, Push: true}, domain.CreateDraft{Title: "T", Labels: []string{"bug"}}, true},
		{"none", domain.RepoPermissions{}, domain.CreateDraft{Title: "T"}, false},
	} {
		f := newCreateFakes()
		f.access.Repo.Permissions = tc.perms
		res := f.commands().CreateIssue(context.Background(), createInput(tc.d))
		if tc.ok != (res.Kind == ResultOperation) {
			t.Errorf("%s: got %+v", tc.name, res)
		}
		if !tc.ok {
			if res.Kind != ResultForbidden || res.Disposition != DispositionNotRecorded || f.called("Access.Token") || f.called("Store.ActorGithubID") {
				t.Errorf("%s: got %+v calls %v", tc.name, res, f.calls)
			}
		}
	}
}

// An unresolved earlier create fails stale pending creates once and retries once; when it is still there the request
// is answered with the earlier operation and nothing is sent.
func TestCreateDuplicate(t *testing.T) {
	boom := errors.New("boom")
	const (
		unresolvedMsg = "an earlier create of this issue is still unresolved; recheck it instead of resubmitting"
		resolvedMsg   = "an earlier create of this issue was resolved while submitting; nothing was sent: check it before resubmitting"
	)
	u := ReserveUnresolvedCreate
	for _, tc := range []struct {
		name     string
		setup    func(*fakes)
		reserves int
		kind     ResultKind
		disp     Disposition
		msg      string
		opID     int64
		err      error
	}{
		{"stale failed, retry reserves", func(f *fakes) { f.reserves, f.staleN = []ReserveResult{u, Reserved}, 1 }, 2, ResultOperation, DispositionPossiblyRecorded, "", 11, nil},
		{"none stale, unresolved", func(f *fakes) { f.reserves, f.unresolved = []ReserveResult{u}, id(9) }, 1, ResultDuplicateCreate, DispositionNotRecorded, unresolvedMsg, 9, nil},
		{"stale failed, still unresolved", func(f *fakes) { f.reserves, f.staleN, f.unresolved = []ReserveResult{u, u}, 2, id(9) }, 2, ResultDuplicateCreate, DispositionNotRecorded, unresolvedMsg, 9, nil},
		{"resolved while submitting", func(f *fakes) { f.reserves, f.latest = []ReserveResult{u}, id(8) }, 1, ResultDuplicateCreate, DispositionNotRecorded, resolvedMsg, 8, nil},
		{"changed while submitting", func(f *fakes) { f.reserves = []ReserveResult{u} }, 1, ResultCreateChanged, DispositionNotRecorded, "", 0, nil},
		{"fail stale error", func(f *fakes) { f.reserves, f.staleErr = []ReserveResult{u}, boom }, 1, ResultInternal, DispositionNone, "", 0, boom},
		{"unresolved lookup error", func(f *fakes) { f.reserves, f.lookupErr = []ReserveResult{u}, boom }, 1, ResultInternal, DispositionNotRecorded, "", 0, boom},
		{"latest lookup error", func(f *fakes) { f.reserves, f.latestErr = []ReserveResult{u}, boom }, 1, ResultInternal, DispositionNotRecorded, "", 0, boom},
	} {
		f := newCreateFakes()
		tc.setup(f)
		res := f.commands().CreateIssue(context.Background(), createInput(metaDraft))
		if res.Kind != tc.kind || res.Disposition != tc.disp || res.Message != tc.msg || (tc.kind == ResultDuplicateCreate && res.OperationID != tc.opID) ||
			!errors.Is(res.Err, tc.err) || (tc.kind == ResultOperation && res.Operation.ID != tc.opID) {
			t.Errorf("%s: got %+v", tc.name, res)
		}
		if n := count(f, "Store.Reserve"); n != tc.reserves {
			t.Errorf("%s: reserved %d times, want %d", tc.name, n, tc.reserves)
		}
		if n := count(f, "Store.FailStalePendingCreate"); n != 1 {
			t.Errorf("%s: FailStalePendingCreate called %d times", tc.name, n)
		}
		if sent := f.called("GitHub.CreateIssue"); sent != (tc.kind == ResultOperation) {
			t.Errorf("%s: sent %v, calls %v", tc.name, sent, f.calls)
		}
	}
}

func TestCreateReplayAndKeyReuse(t *testing.T) {
	f := newCreateFakes()
	f.prior = &PriorOperation{ID: 3, RequestHash: createHash(t, metaDraft)}
	f.op = Operation{State: "succeeded"}
	res := f.commands().CreateIssue(context.Background(), createInput(metaDraft))
	if res.Kind != ResultOperation || res.Created || res.Operation.ID != 3 || res.Disposition != DispositionPossiblyRecorded ||
		!slices.Equal(f.calls, []string{"Access.CheckedAccess", "Store.FindByKey", "Store.Get", "Access.FenceCurrent"}) {
		t.Fatalf("replay: got %+v calls %v", res, f.calls)
	}

	f = newCreateFakes()
	f.prior = &PriorOperation{ID: 3, RequestHash: []byte("other")}
	res = f.commands().CreateIssue(context.Background(), createInput(metaDraft))
	if res.Kind != ResultKeyReuse || res.Disposition != DispositionNotRecorded || f.called("Access.Token") {
		t.Fatalf("reuse: got %+v calls %v", res, f.calls)
	}

	// The update and create hashes differ even for the same key.
	if string(createHash(t, metaDraft)) == string(domain.RequestHash("update", 42, 0, mustJSON(t, metaDraft))) {
		t.Fatal("create hash equals an update hash")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCreateBeforeRecordingFailures(t *testing.T) {
	boom := errors.New("boom")
	unauth := &RemoteError{Kind: domain.Unauthorized, Status: 401, Err: errors.New("401")}
	for _, tc := range []struct {
		name   string
		setup  func(*fakes)
		kind   ResultKind
		disp   Disposition
		logins []string
	}{
		{"access error", func(f *fakes) { f.accessErr = boom }, ResultInternal, DispositionNotRecorded, nil},
		{"not found", func(f *fakes) { f.access.Decision = AccessNotFound }, ResultNotFound, DispositionNotRecorded, nil},
		{"unverified", func(f *fakes) { f.access.Decision = AccessUnverified }, ResultAccessUnverified, DispositionNotRecorded, nil},
		{"moved", func(f *fakes) { f.access.Decision = AccessMoved }, ResultAccessCheckMoved, DispositionNotRecorded, nil},
		{"replay lookup error", func(f *fakes) { f.findErr = boom }, ResultInternal, DispositionNone, nil},
		{"sign in", func(f *fakes) { f.tokenErr = ErrSignInRequired }, ResultSignInRequired, DispositionNotRecorded, nil},
		{"credential", func(f *fakes) { f.tokenErr = boom }, ResultCredentialUnavailable, DispositionNotRecorded, nil},
		{"unassignable", func(f *fakes) {
			f.assign = func(context.Context, string) (bool, error) { return false, nil }
		}, ResultUnassignable, DispositionNotRecorded, []string{"amy"}},
		{"assignable upstream", func(f *fakes) {
			f.assign = func(context.Context, string) (bool, error) { return false, boom }
		}, ResultUpstream, DispositionNotRecorded, nil},
		{"assignable 401", func(f *fakes) {
			f.assign = func(context.Context, string) (bool, error) { return false, unauth }
		}, ResultSignInRequired, DispositionNotRecorded, nil},
		{"reserve error", func(f *fakes) { f.reserveErr = boom }, ResultInternal, DispositionNone, nil},
		{"reserve access moved", func(f *fakes) { f.reserve = ReserveAccessMoved }, ResultSubmitAccessMoved, DispositionNotRecorded, nil},
		{"reserve race vanished", func(f *fakes) { f.reserve = ReserveRace }, ResultInternal, DispositionNone, nil},
		{"mark sent error", func(f *fakes) { f.markErr = boom }, ResultInternal, DispositionPossiblyRecorded, nil},
		{"lost claim", func(f *fakes) { f.lostClaim = true }, ResultOperation, DispositionPossiblyRecorded, nil},
	} {
		f := newCreateFakes()
		tc.setup(f)
		res := f.commands().CreateIssue(context.Background(), createInput(metaDraft))
		if res.Kind != tc.kind || res.Disposition != tc.disp || !slices.Equal(res.Logins, tc.logins) || res.Created {
			t.Errorf("%s: got %+v", tc.name, res)
		}
		if f.called("GitHub.CreateIssue") || f.called("Store.SettleUnderLock") {
			t.Errorf("%s: sent, calls %v", tc.name, f.calls)
		}
		if tc.name == "lost claim" && !f.called("Store.FailStale") {
			t.Errorf("lost claim: calls %v", f.calls)
		}
		if tc.name == "assignable 401" && !f.called("Access.Revoke") {
			t.Errorf("assignable 401: calls %v", f.calls)
		}
		if tc.name == "reserve race vanished" && (res.Err == nil || !strings.Contains(res.Err.Error(), "idempotency key vanished")) {
			t.Errorf("race: got %+v", res)
		}
	}
}

func TestCreateRaceReplays(t *testing.T) {
	f := newCreateFakes()
	f.reserve = ReserveRace
	c := f.commands()
	c.Store = raceStore{fakeStore{f}, &PriorOperation{ID: 3, RequestHash: createHash(t, metaDraft)}}
	res := c.CreateIssue(context.Background(), createInput(metaDraft))
	if res.Kind != ResultOperation || res.Created || res.Operation.ID != 3 || f.called("GitHub.CreateIssue") {
		t.Fatalf("got %+v calls %v", res, f.calls)
	}
}

func TestCreateSettleErrorIsPossiblyRecorded(t *testing.T) {
	f := newCreateFakes()
	c := f.commands()
	c.Store = failingSettle{fakeStore{f}}
	res := c.CreateIssue(context.Background(), createInput(metaDraft))
	if res.Kind != ResultInternal || res.Disposition != DispositionPossiblyRecorded || !errors.Is(res.Err, ErrAlreadyLinked) {
		t.Fatalf("got %+v", res)
	}
}

type failingSettle struct{ fakeStore }

func (s failingSettle) SettleUnderLock(context.Context, Settle, func() (Settlement, error)) error {
	return ErrAlreadyLinked
}
