package application

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/issues/domain"
)

func newStatusFakes() *fakes {
	f := newFakes()
	f.access.Repo.Permissions.Maintain = true
	return f
}

func enableIn() EnableStatusLabelsInput {
	return EnableStatusLabelsInput{UserID: 1, RepoID: 42, Key: "key-1"}
}

// The stored draft (and so the request hash) is exactly what the handler marshalled before the move.
var statusDraft = []byte(`{"labels":["status:todo","status:in-progress","status:review"]}`)

func remote(kind domain.ErrorKind, status int) error {
	return &RemoteError{Kind: kind, Status: status, Err: errors.New("github")}
}

func TestEnableStatusLabelsAllExistingAreReused(t *testing.T) {
	f := newStatusFakes()
	exists := remote(domain.HTTPError, 422)
	f.labelCreateErrs = map[string]error{"status:todo": exists, "status:in-progress": exists, "status:review": exists}
	res := f.commands().EnableStatusLabels(context.Background(), enableIn())
	want := []string{"Access.CheckedAccess", "Store.FindByKey", "Access.Token", "Store.Reserve", "Store.MarkSent", "GitHub.CreateLabel",
		"GitHub.CreateLabel", "GitHub.CreateLabel", "GitHub.LabelExists", "GitHub.LabelExists", "GitHub.LabelExists", "Store.SettleStatusLabels",
		"Store.Get", "Access.FenceCurrent"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	if s := f.statusSettle; s == nil || s.State != "succeeded" || len(s.Outcome.Created) != 0 || s.Outcome.Error != "" || s.Outcome.Unapplied != nil {
		t.Fatalf("status settle %+v", s)
	}
	if res.Kind != ResultOperation || !res.Created || res.Operation.State != "succeeded" || res.Disposition != DispositionPossiblyRecorded {
		t.Fatalf("got %+v", res)
	}
	op := f.newOps[0]
	if op.Kind != "status_labels" || string(op.Draft) != string(statusDraft) || !slices.Equal(op.RequestHash, domain.RequestHash("status_labels", 42, 0, statusDraft)) ||
		op.ContentHash != nil || op.IssueNumber != 0 || op.Key != "key-1" || op.UserID != 1 || op.RepositoryID != 42 {
		t.Fatalf("reserved %+v", op)
	}
}

func TestEnableStatusLabelsCreatesMissing(t *testing.T) {
	f := newStatusFakes()
	f.labelCreateErrs = map[string]error{"status:in-progress": remote(domain.HTTPError, 422)}
	res := f.commands().EnableStatusLabels(context.Background(), enableIn())
	if s := f.statusSettle; s == nil || s.State != "succeeded" || !slices.Equal(s.Outcome.Created, []string{"status:todo", "status:review"}) {
		t.Fatalf("status settle %+v", s)
	}
	if res.Kind != ResultOperation || !res.Created || res.Operation.State != "succeeded" {
		t.Fatalf("got %+v", res)
	}
}

func TestEnableStatusLabelsCreateFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		missing bool
		state   string
		revoke  bool
	}{
		{"5xx with a label still missing is unknown", remote(domain.HTTPError, 502), true, "unknown", false},
		{"5xx with every label read back succeeds", remote(domain.HTTPError, 502), false, "succeeded", false},
		{"4xx with a label missing fails", remote(domain.HTTPError, 403), true, "failed", false},
		{"401 revokes and fails", remote(domain.Unauthorized, 401), true, "failed", true},
	} {
		f := newStatusFakes()
		f.labelCreateErrs = map[string]error{"status:in-progress": tc.err}
		if tc.missing {
			f.missing = map[string]bool{"status:in-progress": true, "status:review": true}
		}
		res := f.commands().EnableStatusLabels(context.Background(), enableIn())
		var created int
		for _, c := range f.calls {
			if c == "GitHub.CreateLabel" {
				created++
			}
		}
		_, msg := classify(tc.err)
		s := f.statusSettle
		if s == nil || s.State != tc.state || s.Outcome.Error != msg || !slices.Equal(s.Outcome.Created, []string{"status:todo"}) || created != 2 {
			t.Errorf("%s: status settle %+v, %d creates", tc.name, s, created)
			continue
		}
		if tc.missing && !slices.Equal(s.Outcome.Unapplied, []string{"create label status:in-progress", "create label status:review"}) {
			t.Errorf("%s: unapplied %v", tc.name, s.Outcome.Unapplied)
		}
		if f.called("Access.Revoke") != tc.revoke {
			t.Errorf("%s: revoke %v, want %v", tc.name, f.called("Access.Revoke"), tc.revoke)
		}
		if res.Kind != ResultOperation || !res.Created || res.Operation.State != tc.state || res.Disposition != DispositionPossiblyRecorded {
			t.Errorf("%s: got %+v", tc.name, res)
		}
	}
}

func TestEnableStatusLabelsForbiddenRecordsNothing(t *testing.T) {
	for _, a := range []Access{triage(), pusher()} {
		f := newFakes()
		f.access = a
		res := f.commands().EnableStatusLabels(context.Background(), enableIn())
		if res.Kind != ResultForbidden || res.Disposition != DispositionNotRecorded || f.called("Access.Token") || f.called("Store.Reserve") {
			t.Fatalf("got %+v calls %v", res, f.calls)
		}
	}
	for _, p := range []domain.RepoPermissions{{Admin: true}, {Maintain: true}} {
		f := newFakes()
		f.access.Repo.Permissions = p
		if res := f.commands().EnableStatusLabels(context.Background(), enableIn()); res.Kind != ResultOperation {
			t.Fatalf("%+v: got %+v", p, res)
		}
	}
}

func TestEnableStatusLabelsReplay(t *testing.T) {
	hash := domain.RequestHash("status_labels", 42, 0, statusDraft)
	t.Run("same key and request answers the stored operation", func(t *testing.T) {
		f := newStatusFakes()
		f.access = pusher() // replay comes before permission
		f.prior = &PriorOperation{ID: 3, RequestHash: hash}
		f.op = Operation{State: "succeeded"}
		res := f.commands().EnableStatusLabels(context.Background(), enableIn())
		want := []string{"Access.CheckedAccess", "Store.FindByKey", "Store.Get", "Access.FenceCurrent"}
		if !slices.Equal(f.calls, want) || res.Kind != ResultOperation || res.Created || res.Operation.ID != 3 || res.Disposition != DispositionPossiblyRecorded {
			t.Fatalf("got %+v calls %v", res, f.calls)
		}
	})
	t.Run("same key with another request is key reuse", func(t *testing.T) {
		f := newStatusFakes()
		f.prior = &PriorOperation{ID: 3, RequestHash: []byte("other")}
		res := f.commands().EnableStatusLabels(context.Background(), enableIn())
		if res.Kind != ResultKeyReuse || res.Disposition != DispositionNotRecorded || f.called("Access.Token") || f.called("Store.Reserve") {
			t.Fatalf("got %+v calls %v", res, f.calls)
		}
	})
	t.Run("a replay read error clears the disposition", func(t *testing.T) {
		f := newStatusFakes()
		f.findErr = errors.New("db")
		if res := f.commands().EnableStatusLabels(context.Background(), enableIn()); res.Kind != ResultInternal || res.Disposition != DispositionNone {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("an insert race replays; a vanished key is internal", func(t *testing.T) {
		f := newStatusFakes()
		f.reserve = ReserveRace
		f.op = Operation{State: "unknown"}
		res := f.commands().EnableStatusLabels(context.Background(), enableIn())
		if res.Kind != ResultInternal || res.Disposition != DispositionNone || f.called("Store.MarkSent") {
			t.Fatalf("vanished key: got %+v calls %v", res, f.calls)
		}
	})
}

// Access moving between the GitHub calls and settlement is decided inside the store's SettleStatusLabels transaction:
// the use case hands it a success and the fence, and answers what was stored (202 when it became unknown).
func TestEnableStatusLabelsAccessMovedBeforeSettlement(t *testing.T) {
	f := newStatusFakes()
	f.statusMoved = true
	res := f.commands().EnableStatusLabels(context.Background(), enableIn())
	if s := f.statusSettle; s == nil || s.State != "succeeded" {
		t.Fatalf("status settle %+v", s)
	}
	if f.settled.State != "unknown" || f.settled.Outcome.Error != "access changed before status labels could be turned on; recheck" {
		t.Fatalf("stored %+v", f.settled)
	}
	if res.Kind != ResultOperation || !res.Created || res.Operation.State != "unknown" {
		t.Fatalf("got %+v", res)
	}
}

func TestEnableStatusLabelsDispositions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fakes)
		kind  ResultKind
		disp  Disposition
		sends bool
	}{
		{"access error", func(f *fakes) { f.accessErr = errors.New("db") }, ResultInternal, DispositionNotRecorded, false},
		{"not found", func(f *fakes) { f.access.Decision = AccessNotFound }, ResultNotFound, DispositionNotRecorded, false},
		{"unverified", func(f *fakes) { f.access.Decision = AccessUnverified }, ResultAccessUnverified, DispositionNotRecorded, false},
		{"check moved", func(f *fakes) { f.access.Decision = AccessMoved }, ResultAccessCheckMoved, DispositionNotRecorded, false},
		{"sign in", func(f *fakes) { f.tokenErr = ErrSignInRequired }, ResultSignInRequired, DispositionNotRecorded, false},
		{"credential", func(f *fakes) { f.tokenErr = errors.New("vault") }, ResultCredentialUnavailable, DispositionNotRecorded, false},
		{"reserve error", func(f *fakes) { f.reserveErr = errors.New("db") }, ResultInternal, DispositionNone, false},
		{"submit access moved", func(f *fakes) { f.reserve = ReserveAccessMoved }, ResultSubmitAccessMoved, DispositionNotRecorded, false},
		{"unexpected reserve", func(f *fakes) { f.reserve = ReserveUnresolvedCreate }, ResultInternal, DispositionNone, false},
		{"mark sent error", func(f *fakes) { f.markErr = errors.New("db") }, ResultInternal, DispositionPossiblyRecorded, false},
		{"settle error", func(f *fakes) { f.settleErr = errors.New("db") }, ResultInternal, DispositionPossiblyRecorded, true},
		{"recorded access moved", func(f *fakes) { f.notCurrent = true }, ResultRecordedAccessMoved, DispositionPossiblyRecorded, true},
	} {
		f := newStatusFakes()
		tc.setup(f)
		res := f.commands().EnableStatusLabels(context.Background(), enableIn())
		if res.Kind != tc.kind || res.Disposition != tc.disp || f.called("GitHub.CreateLabel") != tc.sends {
			t.Errorf("%s: got %+v calls %v", tc.name, res, f.calls)
		}
	}
	t.Run("lost send claim fails the operation and answers 200", func(t *testing.T) {
		f := newStatusFakes()
		f.lostClaim = true
		res := f.commands().EnableStatusLabels(context.Background(), enableIn())
		if res.Kind != ResultOperation || res.Created || res.Disposition != DispositionPossiblyRecorded || !f.called("Store.FailStale") || f.called("GitHub.CreateLabel") {
			t.Fatalf("got %+v calls %v", res, f.calls)
		}
	})
}

func TestDisableStatusLabels(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(f *fakes)
		kind     ResultKind
		disables bool
	}{
		{"allowed turns them off in the store's fenced transaction", func(f *fakes) {}, ResultNoContent, true},
		{"a moved fence answers access moved", func(f *fakes) { f.disableMoved = true }, ResultDisableAccessMoved, true},
		{"a store error is internal", func(f *fakes) { f.disableErr = errors.New("db") }, ResultInternal, true},
		{"without Admin or Maintain it is forbidden", func(f *fakes) { f.access = pusher() }, ResultStatusLabelsForbidden, false},
		{"access error", func(f *fakes) { f.accessErr = errors.New("db") }, ResultInternal, false},
		{"not found", func(f *fakes) { f.access.Decision = AccessNotFound }, ResultNotFound, false},
		{"unverified", func(f *fakes) { f.access.Decision = AccessUnverified }, ResultAccessUnverified, false},
		{"check moved", func(f *fakes) { f.access.Decision = AccessMoved }, ResultAccessCheckMoved, false},
	} {
		f := newStatusFakes()
		tc.setup(f)
		res := f.commands().DisableStatusLabels(context.Background(), 1, 42)
		if res.Kind != tc.kind || res.Disposition != DispositionNone || f.called("Store.DisableStatusLabels") != tc.disables {
			t.Errorf("%s: got %+v calls %v", tc.name, res, f.calls)
		}
		if f.called("GitHub.CreateLabel") || f.called("Store.Reserve") || f.called("Access.Token") {
			t.Errorf("%s: unexpected calls %v", tc.name, f.calls)
		}
	}
}
