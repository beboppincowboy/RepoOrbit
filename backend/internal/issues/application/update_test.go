package application

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

// fakes records every port call, in order, as "Port.Method".
type fakes struct {
	mu    sync.Mutex
	calls []string

	access     Access
	accessErr  error
	notCurrent bool
	tokenErr   error

	issues    []Issue // FetchIssue answers in order; the last one repeats
	fetchErrs []error // matched to issues by call index; nil entries succeed
	fetches   int
	stepErrs  map[string]error
	assign    func(ctx context.Context, login string) (bool, error)

	prior      *PriorOperation
	findErr    error
	reserve    ReserveResult
	reserveErr error
	lostClaim  bool
	markErr    error
	failErr    error
	op         Operation
	settle     Settle
	settled    Settlement

	created    Issue           // CreateIssue's answer
	createErr  error           // CreateIssue's error
	reserves   []ReserveResult // when set, Reserve answers these in order; the last one repeats
	reserveN   int
	newOps     []NewOperation
	staleN     int64 // FailStalePendingCreate's count
	staleErr   error
	unresolved *int64 // FindUnresolvedCreate's id; nil when none
	latest     *int64 // FindLatestCreate's id; nil when none
	lookupErr  error  // FindUnresolvedCreate's error
	latestErr  error

	scopeRepo    int64 // Scope's answer; scopeMissing means not found
	scopeStale   bool
	scopeMissing bool
	scopeErr     error
	fenceErr     error
	getMissing   bool
	settleErr    error           // SettleUnderLock returns it when read succeeded
	missing      map[string]bool // LabelExists answers false for these names
	labelErr     error
	statusSettle *Settlement // what SettleStatusLabels was called with; nil when not called

	labelCreateErrs map[string]error // CreateLabel answers these by label name
	statusMoved     bool             // SettleStatusLabels sees a moved fence, as the store's transaction would
	disableMoved    bool
	disableErr      error
}

func (f *fakes) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakes) called(c string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, c)
}

func (f *fakes) commands() *Commands {
	return &Commands{Access: fakeAccess{f}, GitHub: fakeGitHub{f}, Store: fakeStore{f}}
}

type fakeAccess struct{ f *fakes }

func (a fakeAccess) CheckedAccess(context.Context, int64, int64) (Access, error) {
	a.f.record("Access.CheckedAccess")
	return a.f.access, a.f.accessErr
}
func (a fakeAccess) FenceCurrent(context.Context, Fence) (bool, error) {
	a.f.record("Access.FenceCurrent")
	return !a.f.notCurrent, a.f.fenceErr
}
func (a fakeAccess) Token(context.Context, int64) (string, error) {
	a.f.record("Access.Token")
	return "token", a.f.tokenErr
}
func (a fakeAccess) Revoke(context.Context, int64) { a.f.record("Access.Revoke") }

type fakeGitHub struct{ f *fakes }

func (g fakeGitHub) FetchIssue(context.Context, string, string, int64) (Issue, error) {
	g.f.record("GitHub.FetchIssue")
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	i := g.f.fetches
	g.f.fetches++
	if i < len(g.f.fetchErrs) && g.f.fetchErrs[i] != nil {
		return Issue{}, g.f.fetchErrs[i]
	}
	return g.f.issues[min(i, len(g.f.issues)-1)], nil
}
func (g fakeGitHub) CreateIssue(context.Context, string, string, domain.CreateDraft) (Issue, error) {
	g.f.record("GitHub.CreateIssue")
	return g.f.created, g.f.createErr
}
func (g fakeGitHub) step(name string) error {
	g.f.record("GitHub." + name)
	return g.f.stepErrs[name]
}
func (g fakeGitHub) SetIssueState(_ context.Context, _, _ string, _ int64, state string) error {
	return g.step("SetIssueState")
}
func (g fakeGitHub) AddLabels(context.Context, string, string, int64, []string) error {
	return g.step("AddLabels")
}
func (g fakeGitHub) RemoveLabel(context.Context, string, string, int64, string) error {
	return g.step("RemoveLabel")
}
func (g fakeGitHub) AddAssignees(context.Context, string, string, int64, []string) error {
	return g.step("AddAssignees")
}
func (g fakeGitHub) RemoveAssignees(context.Context, string, string, int64, []string) error {
	return g.step("RemoveAssignees")
}
func (g fakeGitHub) IsAssignable(ctx context.Context, _, _, login string) (bool, error) {
	g.f.record("GitHub.IsAssignable")
	if g.f.assign != nil {
		return g.f.assign(ctx, login)
	}
	return true, nil
}
func (g fakeGitHub) CreateLabel(_ context.Context, _, _, name, _ string) error {
	g.f.record("GitHub.CreateLabel")
	return g.f.labelCreateErrs[name]
}
func (g fakeGitHub) LabelExists(_ context.Context, _, _, name string) (bool, error) {
	g.f.record("GitHub.LabelExists")
	if g.f.labelErr != nil {
		return false, g.f.labelErr
	}
	return !g.f.missing[name], nil
}

type fakeStore struct{ f *fakes }

func (s fakeStore) FindByKey(context.Context, int64, string) (PriorOperation, bool, error) {
	s.f.record("Store.FindByKey")
	if s.f.prior == nil {
		return PriorOperation{}, false, s.f.findErr
	}
	return *s.f.prior, true, s.f.findErr
}
func (s fakeStore) Reserve(_ context.Context, _ Fence, op NewOperation) (int64, ReserveResult, error) {
	s.f.record("Store.Reserve")
	s.f.newOps = append(s.f.newOps, op)
	r := s.f.reserve
	if len(s.f.reserves) > 0 {
		r = s.f.reserves[min(s.f.reserveN, len(s.f.reserves)-1)]
		s.f.reserveN++
	}
	if r != Reserved || s.f.reserveErr != nil {
		return 0, r, s.f.reserveErr
	}
	return 11, Reserved, nil
}
func (s fakeStore) FailStalePendingCreate(context.Context, CreateKey) (int64, error) {
	s.f.record("Store.FailStalePendingCreate")
	return s.f.staleN, s.f.staleErr
}
func (s fakeStore) FindUnresolvedCreate(context.Context, CreateKey) (int64, bool, error) {
	s.f.record("Store.FindUnresolvedCreate")
	if s.f.unresolved == nil {
		return 0, false, s.f.lookupErr
	}
	return *s.f.unresolved, true, s.f.lookupErr
}
func (s fakeStore) FindLatestCreate(context.Context, CreateKey) (int64, bool, error) {
	s.f.record("Store.FindLatestCreate")
	if s.f.latest == nil {
		return 0, false, s.f.latestErr
	}
	return *s.f.latest, true, s.f.latestErr
}
func (s fakeStore) MarkSent(context.Context, int64) (bool, error) {
	s.f.record("Store.MarkSent")
	return !s.f.lostClaim, s.f.markErr
}
func (s fakeStore) FailStale(context.Context, int64) error {
	s.f.record("Store.FailStale")
	return s.f.failErr
}
func (s fakeStore) Scope(context.Context, int64, int64) (int64, bool, bool, error) {
	s.f.record("Store.Scope")
	return s.f.scopeRepo, s.f.scopeStale, !s.f.scopeMissing, s.f.scopeErr
}
func (s fakeStore) Get(_ context.Context, id, _ int64) (Operation, bool, error) {
	s.f.record("Store.Get")
	op := s.f.op
	op.ID = id
	if s.f.settled.State != "" {
		op.State = s.f.settled.State
	}
	return op, !s.f.getMissing, nil
}
func (s fakeStore) ActorGithubID(context.Context, int64) (int64, error) {
	s.f.record("Store.ActorGithubID")
	return 5, nil
}
func (s fakeStore) SettleUnderLock(_ context.Context, st Settle, read func() (Settlement, error)) error {
	s.f.record("Store.SettleUnderLock")
	s.f.settle = st
	res, err := read()
	if err != nil {
		return err
	}
	if s.f.settleErr != nil {
		return s.f.settleErr
	}
	s.f.settled = res
	return nil
}
func (s fakeStore) SettleStatusLabels(_ context.Context, _, _ int64, _ Fence, state string, out domain.Outcome) error {
	s.f.record("Store.SettleStatusLabels")
	s.f.statusSettle = &Settlement{State: state, Outcome: out}
	if state == "succeeded" && s.f.statusMoved {
		state, out.Error = "unknown", "access changed before status labels could be turned on; recheck"
	}
	if s.f.settleErr != nil {
		return s.f.settleErr
	}
	s.f.settled = Settlement{State: state, Outcome: out}
	return nil
}
func (s fakeStore) DisableStatusLabels(context.Context, int64, Fence) (bool, error) {
	s.f.record("Store.DisableStatusLabels")
	return s.f.disableMoved, s.f.disableErr
}

func triage() Access {
	return Access{Decision: AccessAllowed, Repo: Repo{Repo: domain.Repo{HasIssues: true, Permissions: domain.RepoPermissions{Triage: true, Pull: true}}, FullName: "o/r"}, Fence: "fence"}
}

func issue(state string, labels ...string) Issue {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return Issue{View: domain.NewIssueView(7, "https://github.test/o/r/issues/7", "T", state, labels, nil), AuthorID: 5, UpdatedAt: at, InRepository: true, Payload: "payload-" + state}
}

func newFakes() *fakes {
	return &fakes{access: triage(), issues: []Issue{issue("open")}, op: Operation{State: "unknown"}}
}

func input(d domain.UpdateDraft) UpdateIssueInput {
	return UpdateIssueInput{UserID: 1, RepoID: 42, Number: 7, Key: "key-1", Draft: d}
}

func hashOf(t *testing.T, d domain.UpdateDraft) []byte {
	t.Helper()
	draft, err := json.Marshal(&d)
	if err != nil {
		t.Fatal(err)
	}
	return domain.RequestHash("update", 42, 7, draft)
}

var errWrites = []string{"Store.Reserve", "Store.MarkSent", "GitHub.SetIssueState", "GitHub.AddLabels", "GitHub.RemoveLabel", "GitHub.AddAssignees", "GitHub.RemoveAssignees", "Store.SettleUnderLock"}

func assertNoWrites(t *testing.T, f *fakes) {
	t.Helper()
	for _, c := range errWrites {
		if f.called(c) {
			t.Errorf("%s was called; calls %v", c, f.calls)
		}
	}
}

func TestUpdateStaleExpectationConflictsBeforeRecording(t *testing.T) {
	f := newFakes()
	f.issues = []Issue{issue("open", "status:in-progress")}
	d := domain.UpdateDraft{State: "closed", Expect: &domain.Expectation{Status: "Todo", Assignees: []string{}}}
	res := f.commands().UpdateIssue(context.Background(), input(d))
	if res.Kind != ResultConflict || res.Disposition != DispositionNotRecorded {
		t.Fatalf("got %+v", res)
	}
	c := res.Current
	if c.State != "open" || !slices.Equal(c.Labels, []string{"status:in-progress"}) || c.Assignees == nil || len(c.Assignees) != 0 ||
		c.Status != domain.BoardStatus("open", c.Labels) || !c.UpdatedAt.Equal(f.issues[0].UpdatedAt) {
		t.Fatalf("current %+v", c)
	}
	assertNoWrites(t, f)
}

func TestUpdateReplaySameKeyReturnsStoredOperationAfterAccessOnly(t *testing.T) {
	d := domain.UpdateDraft{State: "closed"}
	for _, stale := range []bool{false, true} {
		f := newFakes()
		f.prior = &PriorOperation{ID: 3, RequestHash: hashOf(t, d), StalePending: stale}
		f.op = Operation{State: "succeeded"}
		res := f.commands().UpdateIssue(context.Background(), input(d))
		if res.Kind != ResultOperation || res.Created || res.Operation.ID != 3 {
			t.Fatalf("stale=%v: got %+v", stale, res)
		}
		want := []string{"Access.CheckedAccess", "Store.FindByKey", "Store.Get", "Access.FenceCurrent"}
		if stale {
			want = []string{"Access.CheckedAccess", "Store.FindByKey", "Store.FailStale", "Store.Get", "Access.FenceCurrent"}
		}
		if !slices.Equal(f.calls, want) {
			t.Fatalf("stale=%v: calls %v, want %v", stale, f.calls, want)
		}
	}
}

func TestUpdateReplayNeedsAccessFirst(t *testing.T) {
	d := domain.UpdateDraft{State: "closed"}
	f := newFakes()
	f.access = Access{Decision: AccessNotFound}
	f.prior = &PriorOperation{ID: 3, RequestHash: hashOf(t, d)}
	res := f.commands().UpdateIssue(context.Background(), input(d))
	if res.Kind != ResultNotFound || res.Disposition != DispositionNotRecorded || !slices.Equal(f.calls, []string{"Access.CheckedAccess"}) {
		t.Fatalf("got %+v calls %v", res, f.calls)
	}
}

func TestUpdateReplayDispositions(t *testing.T) {
	d := domain.UpdateDraft{State: "closed"}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name  string
		setup func(*fakes)
		kind  ResultKind
		disp  Disposition
	}{
		{"different payload", func(f *fakes) { f.prior = &PriorOperation{ID: 3, RequestHash: []byte("other")} }, ResultKeyReuse, DispositionNotRecorded},
		{"lookup error", func(f *fakes) { f.findErr = boom }, ResultInternal, DispositionNone},
		{"fail stale error", func(f *fakes) {
			f.prior = &PriorOperation{ID: 3, RequestHash: hashOf(t, d), StalePending: true}
			f.failErr = boom
		}, ResultInternal, DispositionPossiblyRecorded},
		{"fence moved", func(f *fakes) {
			f.prior = &PriorOperation{ID: 3, RequestHash: hashOf(t, d)}
			f.notCurrent = true
		}, ResultRecordedAccessMoved, DispositionPossiblyRecorded},
	} {
		f := newFakes()
		tc.setup(f)
		res := f.commands().UpdateIssue(context.Background(), input(d))
		if res.Kind != tc.kind || res.Disposition != tc.disp {
			t.Errorf("%s: got %+v", tc.name, res)
		}
		if f.called("Access.Token") || f.called("GitHub.FetchIssue") {
			t.Errorf("%s: permission/token/fetch ran: %v", tc.name, f.calls)
		}
	}
}

func TestUpdateAccessDecisions(t *testing.T) {
	retry := time.Now().Add(time.Minute)
	for _, tc := range []struct {
		access Access
		kind   ResultKind
	}{
		{Access{Decision: AccessUnverified, RetryAt: retry}, ResultAccessUnverified},
		{Access{Decision: AccessNotFound}, ResultNotFound},
		{Access{Decision: AccessMoved}, ResultAccessCheckMoved},
	} {
		f := newFakes()
		f.access = tc.access
		res := f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{State: "closed"}))
		if res.Kind != tc.kind || res.Disposition != DispositionNotRecorded || !res.RetryAt.Equal(tc.access.RetryAt) {
			t.Errorf("decision %d: got %+v", tc.access.Decision, res)
		}
		if !slices.Equal(f.calls, []string{"Access.CheckedAccess"}) {
			t.Errorf("decision %d: something was recorded or sent: %v", tc.access.Decision, f.calls)
		}
	}
}

func TestUpdatePermissionAndAuthorFallback(t *testing.T) {
	read := Access{Decision: AccessAllowed, Repo: Repo{Repo: domain.Repo{HasIssues: true, Permissions: domain.RepoPermissions{Pull: true}}, FullName: "o/r"}}
	// Labels need triage: refused before the token.
	f := newFakes()
	f.access = read
	res := f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{Labels: domain.Delta{Add: []string{"bug"}}}))
	if res.Kind != ResultForbidden || res.Disposition != DispositionNotRecorded || f.called("Access.Token") {
		t.Fatalf("labels without triage: %+v %v", res, f.calls)
	}
	// The author may close their own issue with read access.
	f = newFakes()
	f.access = read
	f.issues = []Issue{issue("open"), issue("closed")}
	res = f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{State: "closed"}))
	if res.Kind != ResultOperation || !res.Created || !f.called("Store.ActorGithubID") {
		t.Fatalf("author close: %+v %v", res, f.calls)
	}
	// Someone else's issue, or one without an author, is refused.
	for _, author := range []int64{6, 0} {
		f = newFakes()
		f.access = read
		i := issue("open")
		i.AuthorID = author
		f.issues = []Issue{i}
		res = f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{State: "closed"}))
		if res.Kind != ResultForbidden {
			t.Fatalf("author %d: %+v", author, res)
		}
		assertNoWrites(t, f)
	}
}

func TestUpdateFetchFailures(t *testing.T) {
	pr := issue("open")
	pr.PullRequest = true
	for _, tc := range []struct {
		name   string
		issue  Issue
		err    error
		kind   ResultKind
		revoke bool
	}{
		{"not found", Issue{}, &RemoteError{Kind: domain.NotFound, Status: 404, Err: errors.New("404")}, ResultIssueNotFound, false},
		{"pull request", pr, nil, ResultIssueNotFound, false},
		{"unauthorized", Issue{}, &RemoteError{Kind: domain.Unauthorized, Status: 401, Err: errors.New("401")}, ResultSignInRequired, true},
		{"5xx", Issue{}, &RemoteError{Kind: domain.HTTPError, Status: 502, Err: errors.New("502")}, ResultUpstream, false},
	} {
		f := newFakes()
		f.issues = []Issue{tc.issue}
		f.fetchErrs = []error{tc.err}
		res := f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{State: "closed"}))
		if res.Kind != tc.kind || res.Disposition != DispositionNotRecorded || f.called("Access.Revoke") != tc.revoke {
			t.Errorf("%s: got %+v calls %v", tc.name, res, f.calls)
		}
		assertNoWrites(t, f)
	}
}

func TestUpdateTokenFailures(t *testing.T) {
	for _, tc := range []struct {
		err  error
		kind ResultKind
	}{{ErrSignInRequired, ResultSignInRequired}, {ErrCredentialUnavailable, ResultCredentialUnavailable}} {
		f := newFakes()
		f.tokenErr = tc.err
		if res := f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{State: "closed"})); res.Kind != tc.kind || res.Disposition != DispositionNotRecorded {
			t.Errorf("%v: got %+v", tc.err, res)
		}
	}
}

func TestUpdateUnassignableListsLogins(t *testing.T) {
	f := newFakes()
	f.assign = func(_ context.Context, login string) (bool, error) { return login == "bob", nil }
	res := f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{Assignees: domain.Delta{Add: []string{"amy", "bob", "cid"}}}))
	if res.Kind != ResultUnassignable || res.Disposition != DispositionNotRecorded || !slices.Equal(res.Logins, []string{"amy", "cid"}) {
		t.Fatalf("got %+v", res)
	}
	assertNoWrites(t, f)
}

// A 401 in the concurrent assignability check wins over its siblings' failures and revokes the token.
func TestUpdateAssignabilityUnauthorizedWins(t *testing.T) {
	f := newFakes()
	ready := make(chan struct{})
	f.assign = func(ctx context.Context, login string) (bool, error) {
		if login == "amy" {
			close(ready)
			return false, &RemoteError{Kind: domain.HTTPError, Status: 502, Err: errors.New("502")}
		}
		<-ready
		return false, &RemoteError{Kind: domain.Unauthorized, Status: 401, Err: errors.New("401")}
	}
	res := f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{Assignees: domain.Delta{Add: []string{"amy", "bob"}}}))
	if res.Kind != ResultSignInRequired || !f.called("Access.Revoke") {
		t.Fatalf("got %+v calls %v", res, f.calls)
	}
	assertNoWrites(t, f)
}

func TestUpdateReserveOutcomes(t *testing.T) {
	d := domain.UpdateDraft{State: "closed"}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name  string
		setup func(*fakes)
		kind  ResultKind
		disp  Disposition
		err   string
	}{
		{"insert error", func(f *fakes) { f.reserveErr = boom }, ResultInternal, DispositionNone, "boom"},
		{"access moved", func(f *fakes) { f.reserve = ReserveAccessMoved }, ResultSubmitAccessMoved, DispositionNotRecorded, ""},
		{"race vanished", func(f *fakes) { f.reserve = ReserveRace }, ResultInternal, DispositionNone, "idempotency key vanished"},
		{"race replays", func(f *fakes) { f.reserve = ReserveRace }, ResultOperation, DispositionPossiblyRecorded, ""},
		{"mark sent error", func(f *fakes) { f.markErr = boom }, ResultInternal, DispositionPossiblyRecorded, "mark sent: boom"},
		{"lost claim fail error", func(f *fakes) { f.lostClaim, f.failErr = true, boom }, ResultInternal, DispositionPossiblyRecorded, "boom"},
	} {
		f := newFakes()
		tc.setup(f)
		if tc.name == "race replays" {
			// The first lookup misses; the one after the race finds the concurrent insert.
			c := f.commands()
			c.Store = raceStore{fakeStore{f}, &PriorOperation{ID: 3, RequestHash: hashOf(t, d)}}
			res := c.UpdateIssue(context.Background(), input(d))
			if res.Kind != tc.kind || res.Disposition != tc.disp || res.Created || res.Operation.ID != 3 {
				t.Errorf("%s: got %+v", tc.name, res)
			}
			continue
		}
		res := f.commands().UpdateIssue(context.Background(), input(d))
		if res.Kind != tc.kind || res.Disposition != tc.disp || (tc.err != "" && (res.Err == nil || res.Err.Error() != tc.err)) {
			t.Errorf("%s: got %+v", tc.name, res)
		}
		if f.called("GitHub.SetIssueState") {
			t.Errorf("%s: a step was sent", tc.name)
		}
	}
}

// raceStore misses the key on the first lookup and finds prior on the next.
type raceStore struct {
	fakeStore
	prior *PriorOperation
}

func (s raceStore) FindByKey(ctx context.Context, userID int64, key string) (PriorOperation, bool, error) {
	if s.f.called("Store.Reserve") {
		s.f.prior = s.prior
	}
	return s.fakeStore.FindByKey(ctx, userID, key)
}

func TestUpdateLostSendClaimFailsStaleAndAnswers200(t *testing.T) {
	f := newFakes()
	f.lostClaim = true
	f.op = Operation{State: "failed"}
	res := f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{State: "closed"}))
	if res.Kind != ResultOperation || res.Created || res.Operation.ID != 11 || res.Operation.State != "failed" || !f.called("Store.FailStale") {
		t.Fatalf("got %+v calls %v", res, f.calls)
	}
	if f.called("GitHub.SetIssueState") || f.called("Store.SettleUnderLock") {
		t.Fatalf("sent after a lost claim: %v", f.calls)
	}
}

// Reopen succeeds, adding labels fails: a 5xx leaves the update unknown with AllStepsAnswered false; a 4xx fails it
// with AllStepsAnswered true. Either way the unapplied label is listed and settlement runs under the issue lock.
func TestUpdatePartialFailure(t *testing.T) {
	for _, tc := range []struct {
		status   int
		state    string
		answered bool
	}{
		{502, "unknown", false},
		{422, "failed", true},
	} {
		f := newFakes()
		f.issues = []Issue{issue("closed"), issue("open")}
		f.stepErrs = map[string]error{"AddLabels": &RemoteError{Kind: domain.HTTPError, Status: tc.status, Err: errors.New("x")}}
		res := f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{State: "open", Labels: domain.Delta{Add: []string{"bug"}}}))
		s := f.settled
		if s.State != tc.state || s.Outcome.AllStepsAnswered != tc.answered || !slices.Equal(s.Outcome.Unapplied, []string{"add label bug"}) || s.Confirmed != "payload-open" {
			t.Errorf("status %d: settled %+v", tc.status, s)
		}
		if f.settle != (Settle{OperationID: 11, RepositoryID: 42, Fence: "fence", LockNumber: 7}) {
			t.Errorf("status %d: settle %+v", tc.status, f.settle)
		}
		if res.Kind != ResultOperation || !res.Created || res.Operation.State != tc.state {
			t.Errorf("status %d: got %+v", tc.status, res)
		}
		order := []string{"Store.Reserve", "Store.MarkSent", "GitHub.SetIssueState", "GitHub.AddLabels", "Store.SettleUnderLock", "GitHub.FetchIssue"}
		var got []string
		for _, c := range f.calls {
			if slices.Contains(order, c) && !(c == "GitHub.FetchIssue" && len(got) == 0) {
				got = append(got, c)
			}
		}
		if !slices.Equal(got, order) {
			t.Errorf("status %d: order %v", tc.status, got)
		}
	}
}

// Ported from apply_update_test.go: AllStepsAnswered is true exactly when classify calls the failing step "failed"
// (or nothing failed), and the update stops at the first error.
func TestApplyStepsAllStepsAnswered(t *testing.T) {
	for _, tc := range []struct {
		err      error
		answered bool
		msg      string
	}{
		{nil, true, ""},
		{&RemoteError{Kind: domain.NotFound, Status: 404, Err: errors.New("x")}, true, "GitHub rejected the request (status 404)"},
		{&RemoteError{Kind: domain.HTTPError, Status: 422, Err: errors.New("x")}, true, "GitHub rejected the request (status 422)"},
		{&RemoteError{Kind: domain.HTTPError, Status: 502, Err: errors.New("x")}, false, "GitHub answered status 502; the change may or may not have been applied"},
		{&RemoteError{Kind: domain.NotSent, Err: errors.New("x")}, true, "the request could not be prepared; nothing was sent to GitHub"},
		{errors.New("connection reset"), false, "no reliable answer from GitHub; the change may or may not have been applied"},
	} {
		f := newFakes()
		f.stepErrs = map[string]error{"SetIssueState": tc.err, "AddLabels": tc.err}
		out := f.commands().applySteps(context.Background(), 1, "token", "o/r", 7, domain.UpdateDraft{State: "open", Labels: domain.Delta{Add: []string{"bug"}}})
		if out.AllStepsAnswered != tc.answered || out.Error != tc.msg {
			t.Errorf("%v: answered=%v error=%q", tc.err, out.AllStepsAnswered, out.Error)
		}
		want := []string{"GitHub.SetIssueState", "GitHub.AddLabels"}
		if tc.err != nil {
			want = want[:1] // stopped at the failing reopen
		}
		if !slices.Equal(f.calls, want) {
			t.Errorf("%v: calls %v, want %v", tc.err, f.calls, want)
		}
	}
}

// A 401 on any step revokes the token; so does a 401 on the read-back, after settlement.
func TestUpdateUnauthorizedRevokes(t *testing.T) {
	unauth := &RemoteError{Kind: domain.Unauthorized, Status: 401, Err: errors.New("401")}
	for _, step := range []string{"SetIssueState", "AddLabels", "RemoveLabel", "RemoveAssignees", "AddAssignees"} {
		f := newFakes()
		f.issues = []Issue{issue("closed", "old"), issue("closed", "old")}
		f.stepErrs = map[string]error{step: unauth}
		f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{State: "open", Labels: domain.Delta{Add: []string{"bug"}, Remove: []string{"old"}},
			Assignees: domain.Delta{Add: []string{"amy"}, Remove: []string{"bob"}}}))
		if !f.called("Access.Revoke") || f.settled.Outcome.Error != "GitHub rejected the request (status 401)" {
			t.Errorf("%s: calls %v outcome %+v", step, f.calls, f.settled.Outcome)
		}
	}
	f := newFakes()
	f.fetchErrs = []error{nil, unauth}
	res := f.commands().UpdateIssue(context.Background(), input(domain.UpdateDraft{State: "closed"}))
	calls := strings.Join(f.calls, ",")
	if !strings.Contains(calls, "Store.SettleUnderLock,GitHub.FetchIssue,Access.Revoke") || f.settled.State != "unknown" || f.settled.Confirmed != nil ||
		f.settled.Outcome.Error != "Reading the result failed: GitHub rejected the request (status 401)" || res.Kind != ResultOperation {
		t.Fatalf("read-back 401: calls %v settled %+v res %+v", f.calls, f.settled, res)
	}
}
