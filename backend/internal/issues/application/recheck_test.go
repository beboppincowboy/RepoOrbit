package application

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

var sentAt = time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)

func recheckIn(number int64) RecheckOperationInput {
	return RecheckOperationInput{UserID: 1, OperationID: 11, IssueNumber: number}
}

// newRecheckFakes holds an unknown create sent at sentAt whose issue #9 GitHub shows exactly as drafted.
func newRecheckFakes() *fakes {
	f := newFakes()
	f.access = pusher()
	f.scopeRepo = 42
	draft, _ := json.Marshal(&domain.CreateDraft{Title: "T", Body: "B", Labels: []string{"bug"}})
	f.op = Operation{RepositoryID: 42, Kind: "create", State: "unknown", ContentHash: domain.ContentHash("T", "B"), HasRequestedMetadata: true,
		Draft: draft, SentAt: sentAt, ActorGithubID: 5}
	f.issues = []Issue{{View: domain.NewIssueView(9, "https://github.test/o/r/issues/9", "T", "open", []string{"bug"}, nil), AuthorID: 5, Body: "B",
		CreatedAt: sentAt.Add(time.Second), InRepository: true, Payload: "fetched"}}
	return f
}

func newUpdateRecheckFakes(t *testing.T, sent domain.Outcome) *fakes {
	t.Helper()
	f := newRecheckFakes()
	draft, _ := json.Marshal(&domain.UpdateDraft{Labels: domain.Delta{Add: []string{"bug"}}})
	outcome, _ := json.Marshal(&sent)
	f.op = Operation{RepositoryID: 42, Kind: "update", State: "unknown", IssueNumber: 7, Draft: draft, Outcome: outcome, SentAt: sentAt, ActorGithubID: 5}
	return f
}

func newStatusRecheckFakes(sent time.Time) *fakes {
	f := newRecheckFakes()
	f.access.Repo.Permissions.Maintain = true
	f.op = Operation{RepositoryID: 42, Kind: "status_labels", State: "unknown", Outcome: []byte(`{"created":["status:todo"],"error":"x"}`), SentAt: sent}
	return f
}

// assertRecheckDisposition checks the recheck header rule: only respond's possibly-recorded results carry one.
func assertRecheckDisposition(t *testing.T, name string, res Result, possiblyRecorded bool) {
	t.Helper()
	want := DispositionNone
	if possiblyRecorded {
		want = DispositionPossiblyRecorded
	}
	if res.Disposition != want {
		t.Errorf("%s: disposition %q, want %q", name, res.Disposition, want)
	}
}

func TestRecheckNotUnknownReturnsViewUnchanged(t *testing.T) {
	f := newRecheckFakes()
	f.op.State = "succeeded"
	res := f.commands().RecheckOperation(context.Background(), recheckIn(0))
	want := []string{"Store.Scope", "Access.CheckedAccess", "Store.Get", "Access.FenceCurrent"}
	if res.Kind != ResultOperation || res.Created || res.Operation.State != "succeeded" || res.Operation.ID != 11 || !slices.Equal(f.calls, want) {
		t.Fatalf("got %+v calls %v", res, f.calls)
	}
	assertRecheckDisposition(t, "not unknown", res, false)
}

func TestRecheckStalePendingFailsBeforeGet(t *testing.T) {
	f := newRecheckFakes()
	f.scopeStale = true
	f.op.State = "failed"
	f.commands().RecheckOperation(context.Background(), recheckIn(0))
	want := []string{"Store.Scope", "Access.CheckedAccess", "Store.FailStale", "Store.Get", "Access.FenceCurrent"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
}

func TestRecheckLoadFailures(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name  string
		setup func(*fakes)
		kind  ResultKind
	}{
		{"scope missing", func(f *fakes) { f.scopeMissing = true }, ResultNotFound},
		{"scope error", func(f *fakes) { f.scopeErr = boom }, ResultInternal},
		{"access not found", func(f *fakes) { f.access = Access{Decision: AccessNotFound} }, ResultNotFound},
		{"access unverified", func(f *fakes) { f.access = Access{Decision: AccessUnverified, RetryAt: sentAt} }, ResultAccessUnverified},
		{"access check moved", func(f *fakes) { f.access = Access{Decision: AccessMoved} }, ResultAccessCheckMoved},
		{"access error", func(f *fakes) { f.accessErr = boom }, ResultInternal},
		{"fail stale error", func(f *fakes) { f.scopeStale, f.failErr = true, boom }, ResultInternal},
		{"get missing", func(f *fakes) { f.getMissing = true }, ResultNotFound},
		{"fence error", func(f *fakes) { f.fenceErr = boom }, ResultInternal},
		{"fence moved", func(f *fakes) { f.notCurrent = true }, ResultLoadAccessMoved},
	} {
		f := newRecheckFakes()
		tc.setup(f)
		res := f.commands().RecheckOperation(context.Background(), recheckIn(9))
		if res.Kind != tc.kind {
			t.Errorf("%s: got %+v", tc.name, res)
		}
		if tc.kind == ResultAccessUnverified && !res.RetryAt.Equal(sentAt) {
			t.Errorf("%s: RetryAt %v", tc.name, res.RetryAt)
		}
		assertRecheckDisposition(t, tc.name, res, false)
		if f.called("Access.Token") {
			t.Errorf("%s: token loaded: %v", tc.name, f.calls)
		}
	}
}

func TestRecheckCreateNeedsIssueNumber(t *testing.T) {
	f := newRecheckFakes()
	res := f.commands().RecheckOperation(context.Background(), recheckIn(0))
	if res.Kind != ResultIssueNumberRequired || f.called("Access.Token") {
		t.Fatalf("got %+v calls %v", res, f.calls)
	}
	assertRecheckDisposition(t, "issue number", res, false)
}

func TestRecheckTokenFailures(t *testing.T) {
	for _, tc := range []struct {
		err  error
		kind ResultKind
	}{{ErrSignInRequired, ResultSignInRequired}, {ErrCredentialUnavailable, ResultCredentialUnavailable}} {
		f := newRecheckFakes()
		f.tokenErr = tc.err
		res := f.commands().RecheckOperation(context.Background(), recheckIn(9))
		if res.Kind != tc.kind || f.called("Store.SettleUnderLock") {
			t.Errorf("%v: got %+v calls %v", tc.err, res, f.calls)
		}
		assertRecheckDisposition(t, tc.err.Error(), res, false)
	}
}

func TestRecheckCreateVerifiedSucceedsLinked(t *testing.T) {
	f := newRecheckFakes()
	res := f.commands().RecheckOperation(context.Background(), recheckIn(9))
	want := []string{"Store.Scope", "Access.CheckedAccess", "Store.Get", "Access.FenceCurrent", "Access.Token", "Store.SettleUnderLock",
		"GitHub.FetchIssue", "Store.Get", "Access.FenceCurrent"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	if f.settle != (Settle{OperationID: 11, RepositoryID: 42, Fence: "fence", LockNumber: 9, Link: true}) {
		t.Errorf("settle %+v", f.settle)
	}
	s := f.settled
	if s.State != "succeeded" || s.Confirmed != "fetched" || s.Outcome.Issue != f.issues[0].View || len(s.Outcome.Unapplied) != 0 {
		t.Errorf("settled %+v", s)
	}
	if res.Kind != ResultOperation || res.Created || res.Operation.State != "succeeded" {
		t.Fatalf("got %+v", res)
	}
	assertRecheckDisposition(t, "verified", res, true)
}

// A create whose draft was wiped cannot verify its labels or assignees, so it fails with the fixed reason.
func TestRecheckCreateWipedDraftWithMetadataFails(t *testing.T) {
	f := newRecheckFakes()
	f.op.Draft = nil
	f.commands().RecheckOperation(context.Background(), recheckIn(9))
	if s := f.settled; s.State != "failed" || !slices.Equal(s.Outcome.Unapplied, []string{"requested labels or assignees (unverifiable: the draft was removed)"}) {
		t.Fatalf("settled %+v", s)
	}
}

func TestRecheckCreateMismatchesConflictWithUnchangedView(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakes)
	}{
		{"not found", func(f *fakes) {
			f.fetchErrs = []error{&RemoteError{Kind: domain.NotFound, Status: 404, Err: errors.New("404")}}
		}},
		{"pull request", func(f *fakes) { f.issues[0].PullRequest = true }},
		{"no author", func(f *fakes) { f.issues[0].AuthorID = 0 }},
		{"other author", func(f *fakes) { f.issues[0].AuthorID = 6 }},
		{"other repository", func(f *fakes) { f.issues[0].InRepository = false }},
		{"content hash", func(f *fakes) { f.issues[0].Body = "changed" }},
		{"never sent", func(f *fakes) { f.op.SentAt = time.Time{} }},
		{"created before send", func(f *fakes) { f.issues[0].CreatedAt = sentAt.Add(-6 * time.Second) }},
		{"already linked", func(f *fakes) { f.settleErr = ErrAlreadyLinked }},
	} {
		f := newRecheckFakes()
		tc.setup(f)
		res := f.commands().RecheckOperation(context.Background(), recheckIn(9))
		if res.Kind != ResultMismatch || res.Operation.State != "unknown" || res.Operation.ID != 11 {
			t.Errorf("%s: got %+v", tc.name, res)
		}
		if f.calls[len(f.calls)-1] != "Access.FenceCurrent" || f.settled.State != "" || f.called("Access.Revoke") {
			t.Errorf("%s: calls %v settled %+v", tc.name, f.calls, f.settled)
		}
		assertRecheckDisposition(t, tc.name, res, false)
	}
}

// The creation-time check allows five seconds of clock skew.
func TestRecheckCreateWithinSkewVerifies(t *testing.T) {
	f := newRecheckFakes()
	f.issues[0].CreatedAt = sentAt.Add(-4 * time.Second)
	if res := f.commands().RecheckOperation(context.Background(), recheckIn(9)); res.Kind != ResultOperation || f.settled.State != "succeeded" {
		t.Fatalf("got %+v settled %+v", res, f.settled)
	}
}

func TestRecheckMismatchFenceChecks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakes)
		kind  ResultKind
	}{
		{"moved", func(f *fakes) { f.notCurrent = true }, ResultLoadAccessMoved},
		{"error", func(f *fakes) { f.fenceErr = errors.New("boom") }, ResultInternal},
	} {
		f := newRecheckFakes()
		f.issues[0].AuthorID = 6
		res := fenceAfterLoad(f, tc.setup).RecheckOperation(context.Background(), recheckIn(9))
		if res.Kind != tc.kind {
			t.Errorf("%s: got %+v", tc.name, res)
		}
		assertRecheckDisposition(t, tc.name, res, false)
	}
}

// fenceAfterLoad lets the load's fence check pass and applies setup to the fence checks after the token was loaded.
func fenceAfterLoad(f *fakes, setup func(*fakes)) *Commands {
	c := f.commands()
	c.Access = laterFence{fakeAccess{f}, setup}
	return c
}

type laterFence struct {
	fakeAccess
	setup func(*fakes)
}

func (l laterFence) FenceCurrent(ctx context.Context, fence Fence) (bool, error) {
	if l.f.called("Access.Token") {
		l.setup(l.f)
	}
	return l.fakeAccess.FenceCurrent(ctx, fence)
}

func TestRecheckFetchErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		kind   ResultKind
		revoke bool
	}{
		{"401", &RemoteError{Kind: domain.Unauthorized, Status: 401, Err: errors.New("401")}, ResultSignInRequired, true},
		{"5xx", &RemoteError{Kind: domain.HTTPError, Status: 502, Err: errors.New("502")}, ResultUpstream, false},
		{"not sent", &RemoteError{Kind: domain.NotSent, Err: errors.New("dial")}, ResultUpstream, false},
	} {
		f := newRecheckFakes()
		f.fetchErrs = []error{tc.err}
		res := f.commands().RecheckOperation(context.Background(), recheckIn(9))
		if res.Kind != tc.kind || f.called("Access.Revoke") != tc.revoke || f.settled.State != "" {
			t.Errorf("%s: got %+v calls %v", tc.name, res, f.calls)
		}
		assertRecheckDisposition(t, tc.name, res, false)
	}
}

func TestRecheckSettleErrorIsInternal(t *testing.T) {
	f := newRecheckFakes()
	f.settleErr = errors.New("deadlock")
	res := f.commands().RecheckOperation(context.Background(), recheckIn(9))
	if res.Kind != ResultInternal || f.calls[len(f.calls)-1] != "GitHub.FetchIssue" {
		t.Fatalf("got %+v calls %v", res, f.calls)
	}
	assertRecheckDisposition(t, "settle error", res, false)
}

// A create's stored draft that does not parse aborts the settlement as an internal error, not a mismatch.
func TestRecheckCreateBadDraftIsInternal(t *testing.T) {
	f := newRecheckFakes()
	f.op.Draft = []byte(`{`)
	if res := f.commands().RecheckOperation(context.Background(), recheckIn(9)); res.Kind != ResultInternal {
		t.Fatalf("got %+v", res)
	}
}

func TestRecheckUpdate(t *testing.T) {
	t.Run("verified uses the stored number and carries the sent outcome", func(t *testing.T) {
		f := newUpdateRecheckFakes(t, domain.Outcome{Error: "step failed", AllStepsAnswered: false})
		f.issues[0].View = domain.NewIssueView(7, "u", "T", "open", []string{"bug"}, nil)
		res := f.commands().RecheckOperation(context.Background(), recheckIn(99))
		if f.settle != (Settle{OperationID: 11, RepositoryID: 42, Fence: "fence", LockNumber: 7, Link: false}) {
			t.Errorf("settle %+v", f.settle)
		}
		if s := f.settled; s.State != "succeeded" || s.Outcome.Error != "step failed" || s.Outcome.AllStepsAnswered || s.Confirmed != "fetched" {
			t.Errorf("settled %+v", s)
		}
		if res.Kind != ResultOperation || res.Operation.State != "succeeded" {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("unapplied with every step answered fails", func(t *testing.T) {
		f := newUpdateRecheckFakes(t, domain.Outcome{Error: "422", AllStepsAnswered: true})
		f.issues[0].View = domain.NewIssueView(7, "u", "T", "open", nil, nil)
		f.commands().RecheckOperation(context.Background(), recheckIn(0))
		if s := f.settled; s.State != "failed" || !s.Outcome.AllStepsAnswered || !slices.Equal(s.Outcome.Unapplied, []string{"add label bug"}) {
			t.Errorf("settled %+v", s)
		}
	})
	for _, tc := range []struct {
		name  string
		setup func(*fakes)
	}{
		{"unapplied with an unanswered step", func(f *fakes) { f.issues[0].View = domain.NewIssueView(7, "u", "T", "open", nil, nil) }},
		{"wiped draft", func(f *fakes) { f.op.Draft = nil }},
		{"bad draft", func(f *fakes) { f.op.Draft = []byte(`{`) }},
		{"bad outcome", func(f *fakes) { f.op.Outcome = []byte(`{`) }},
	} {
		f := newUpdateRecheckFakes(t, domain.Outcome{})
		tc.setup(f)
		res := f.commands().RecheckOperation(context.Background(), recheckIn(0))
		if res.Kind != ResultMismatch || res.Operation.State != "unknown" || f.settled.State != "" {
			t.Errorf("%s: got %+v settled %+v", tc.name, res, f.settled)
		}
	}
}

func TestRecheckStatusLabels(t *testing.T) {
	t.Run("inside the grace window a missing label stays unknown", func(t *testing.T) {
		f := newStatusRecheckFakes(time.Now())
		f.missing = map[string]bool{"status:review": true}
		res := f.commands().RecheckOperation(context.Background(), recheckIn(0))
		if res.Kind != ResultMismatch || res.Operation.State != "unknown" || f.statusSettle != nil || f.calls[len(f.calls)-1] != "Access.FenceCurrent" {
			t.Fatalf("got %+v calls %v", res, f.calls)
		}
		assertRecheckDisposition(t, "grace", res, false)
	})
	t.Run("inside the grace window a moved fence answers access moved", func(t *testing.T) {
		f := newStatusRecheckFakes(time.Now())
		f.missing = map[string]bool{"status:review": true}
		res := fenceAfterLoad(f, func(f *fakes) { f.notCurrent = true }).RecheckOperation(context.Background(), recheckIn(0))
		if res.Kind != ResultLoadAccessMoved || f.statusSettle != nil {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("after the grace window verified labels succeed", func(t *testing.T) {
		f := newStatusRecheckFakes(time.Now().Add(-3 * time.Minute))
		res := f.commands().RecheckOperation(context.Background(), recheckIn(0))
		want := []string{"Store.Scope", "Access.CheckedAccess", "Store.Get", "Access.FenceCurrent", "Access.Token", "GitHub.LabelExists",
			"GitHub.LabelExists", "GitHub.LabelExists", "Store.SettleStatusLabels", "Store.Get", "Access.FenceCurrent"}
		if !slices.Equal(f.calls, want) {
			t.Fatalf("calls %v, want %v", f.calls, want)
		}
		if s := f.statusSettle; s == nil || s.State != "succeeded" || !slices.Equal(s.Outcome.Created, []string{"status:todo"}) || s.Outcome.Error != "" {
			t.Fatalf("status settle %+v", s)
		}
		if res.Kind != ResultOperation || res.Created || res.Operation.State != "succeeded" {
			t.Fatalf("got %+v", res)
		}
		assertRecheckDisposition(t, "verified", res, true)
	})
	t.Run("verified labels succeed inside the grace window too", func(t *testing.T) {
		f := newStatusRecheckFakes(time.Now())
		f.commands().RecheckOperation(context.Background(), recheckIn(0))
		if s := f.statusSettle; s == nil || s.State != "succeeded" {
			t.Fatalf("status settle %+v", s)
		}
	})
	t.Run("after the grace window a missing label fails", func(t *testing.T) {
		f := newStatusRecheckFakes(time.Now().Add(-3 * time.Minute))
		f.missing = map[string]bool{"status:review": true}
		f.commands().RecheckOperation(context.Background(), recheckIn(0))
		if s := f.statusSettle; s == nil || s.State != "failed" || !slices.Equal(s.Outcome.Unapplied, []string{"create label status:review"}) {
			t.Fatalf("status settle %+v", s)
		}
	})
	t.Run("never sent skips the grace window", func(t *testing.T) {
		f := newStatusRecheckFakes(time.Time{})
		f.missing = map[string]bool{"status:review": true}
		f.commands().RecheckOperation(context.Background(), recheckIn(0))
		if s := f.statusSettle; s == nil || s.State != "failed" {
			t.Fatalf("status settle %+v", s)
		}
	})
	t.Run("a read failure, even a 401, answers 503 without revoking", func(t *testing.T) {
		f := newStatusRecheckFakes(time.Now().Add(-3 * time.Minute))
		f.labelErr = &RemoteError{Kind: domain.Unauthorized, Status: 401, Err: errors.New("401")}
		res := f.commands().RecheckOperation(context.Background(), recheckIn(0))
		if res.Kind != ResultUpstream || f.called("Access.Revoke") || f.statusSettle != nil {
			t.Fatalf("got %+v calls %v", res, f.calls)
		}
		assertRecheckDisposition(t, "read failure", res, false)
	})
	t.Run("without Admin or Maintain it is forbidden", func(t *testing.T) {
		f := newStatusRecheckFakes(time.Now().Add(-3 * time.Minute))
		f.access = pusher()
		res := f.commands().RecheckOperation(context.Background(), recheckIn(0))
		if res.Kind != ResultForbidden || f.called("Access.Token") || f.called("GitHub.LabelExists") {
			t.Fatalf("got %+v calls %v", res, f.calls)
		}
		assertRecheckDisposition(t, "forbidden", res, false)
	})
}

func TestReadStatusLabels(t *testing.T) {
	f := newFakes()
	f.labelErr = &RemoteError{Kind: domain.HTTPError, Status: 502, Err: errors.New("502")}
	state, out := f.commands().ReadStatusLabels(context.Background(), "token", "o/r", domain.Outcome{Error: "create failed.", Unapplied: []string{"stale"}})
	_, msg := domain.Classify(domain.HTTPError, 502)
	if state != "unknown" || out.Error != "create failed. Reading the labels back failed: "+msg || out.Unapplied != nil {
		t.Fatalf("state %q out %+v", state, out)
	}
}
