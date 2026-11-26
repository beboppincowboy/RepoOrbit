package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestValidation(t *testing.T) {
	for _, tc := range []struct {
		draft CreateDraft
		want  string
	}{
		{CreateDraft{Title: "  "}, "title must be 1 to 256 characters"},
		{CreateDraft{Title: "x", Labels: []string{"bug", "BUG"}}, `duplicate label "BUG"`},
		{CreateDraft{Title: "x", Labels: []string{" bad"}}, `invalid label " bad"`},
		{CreateDraft{Title: "x", Assignees: []string{"bad login"}}, `invalid assignee "bad login"`},
		{CreateDraft{Title: "x", Labels: make([]string, 11)}, "at most 10 labels"},
	} {
		if err := tc.draft.Validate(); err == nil || err.Error() != tc.want {
			t.Errorf("got %v, want %s", err, tc.want)
		}
	}
	d := CreateDraft{Title: "  title  "}
	if err := d.Validate(); err != nil || d.Title != "title" {
		t.Fatalf("normalization: %+v %v", d, err)
	}
	for _, tc := range []struct {
		d    UpdateDraft
		want string
	}{{UpdateDraft{}, "no change requested"}, {UpdateDraft{Labels: Delta{Add: []string{"Bug"}, Remove: []string{"bug"}}}, `"Bug" is both added and removed`}} {
		if err := tc.d.Validate(); err == nil || err.Error() != tc.want {
			t.Errorf("got %v, want %s", err, tc.want)
		}
	}
}
func TestExpectationAndBoard(t *testing.T) {
	var none *Expectation
	if !none.Matches(&IssueView{}) {
		t.Fatal("nil expectation must match")
	}
	e := &Expectation{Status: "in_review", Assignees: []string{"ADA", "bob"}, StatusLabels: []string{"status:review", "status:todo"}}
	i := &IssueView{State: "open", Labels: []string{"STATUS:TODO", "status:review", "bug"}, Assignees: []string{"Bob", "ada"}}
	if !e.Matches(i) {
		t.Fatal("same sets should match")
	}
	i.State = "closed"
	if e.Matches(i) {
		t.Fatal("changed state matched")
	}
	for _, tc := range []struct {
		state  string
		labels []string
		want   string
	}{{"closed", nil, "done"}, {"open", []string{"status:todo", "STATUS:REVIEW"}, "in_review"}, {"open", []string{"bug"}, "backlog"}} {
		if got := BoardStatus(tc.state, tc.labels); got != tc.want {
			t.Errorf("board = %q want %q", got, tc.want)
		}
	}
}
func TestRequestHashInputs(t *testing.T) {
	d := []byte(`{"state":"open"}`)
	base := RequestHash("update", 42, 7, d)
	for _, v := range [][]byte{RequestHash("create", 42, 7, d), RequestHash("update", 43, 7, d), RequestHash("update", 42, 8, d)} {
		if bytes.Equal(base, v) {
			t.Fatal("different request hashed identically")
		}
	}
}
func TestOutcomeJSON(t *testing.T) {
	b, _ := json.Marshal(Outcome{Unapplied: []string{"add label bug"}})
	if !strings.Contains(string(b), `"unapplied"`) {
		t.Fatalf("wire format: %s", b)
	}
}
func TestPlanUpdate(t *testing.T) {
	d := UpdateDraft{State: "open", Labels: Delta{Add: []string{"a"}, Remove: []string{"b", "c"}}, Assignees: Delta{Remove: []string{"d"}, Add: []string{"e"}}}
	got := PlanUpdate(d)
	want := []Step{{Kind: Reopen}, {Kind: AddLabels, Names: []string{"a"}}, {Kind: RemoveLabel, Names: []string{"b"}}, {Kind: RemoveLabel, Names: []string{"c"}}, {Kind: RemoveAssignees, Names: []string{"d"}}, {Kind: AddAssignees, Names: []string{"e"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	d.State = "closed"
	got = PlanUpdate(d)
	if got[len(got)-1].Kind != Close {
		t.Fatalf("close must be last: %#v", got)
	}
	// Only requested steps: no state change means no reopen or close.
	got = PlanUpdate(UpdateDraft{Assignees: Delta{Add: []string{"e"}}})
	if want := []Step{{Kind: AddAssignees, Names: []string{"e"}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		kind          ErrorKind
		status        int
		state, reason string
	}{
		{NotSent, 0, "failed", "the request could not be prepared; nothing was sent to GitHub"},
		{Unauthorized, 401, "failed", "GitHub rejected the request (status 401)"},
		{NotFound, 404, "failed", "GitHub rejected the request (status 404)"},
		{HTTPError, 422, "failed", "GitHub rejected the request (status 422)"},
		{HTTPError, 500, "unknown", "GitHub answered status 500; the change may or may not have been applied"},
		{OtherError, 0, "unknown", "no reliable answer from GitHub; the change may or may not have been applied"},
	} {
		state, reason := Classify(tc.kind, tc.status)
		if state != tc.state || reason != tc.reason {
			t.Errorf("%v %d: %q %q", tc.kind, tc.status, state, reason)
		}
	}
}
func TestPermissionMatrix(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		p                                RepoPermissions
		create, metadata, manage, status bool
	}{
		{"read", RepoPermissions{Pull: true}, true, false, false, false},
		{"triage", RepoPermissions{Triage: true}, true, false, true, false},
		{"push", RepoPermissions{Push: true}, true, true, true, false},
		{"maintain", RepoPermissions{Maintain: true}, true, true, true, true},
		{"admin", RepoPermissions{Admin: true}, true, true, true, true},
		{"none", RepoPermissions{}, false, false, false, false},
	} {
		r := Repo{HasIssues: true, Permissions: tc.p}
		if CanCreate(r) != tc.create || CanCreateWithMetadata(r) != tc.metadata || CanManage(r) != tc.manage || CanChangeStatusLabels(r) != tc.status || !writable(r) {
			t.Errorf("%s permission mismatch", tc.name)
		}
		r.Archived = true
		if writable(r) || CanCreate(r) || CanManage(r) || CanChangeStatusLabels(r) {
			t.Errorf("%s archived writable", tc.name)
		}
	}
}
