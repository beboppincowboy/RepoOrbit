package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	maxTitle = 256   // GitHub's title limit, in characters
	maxBody  = 65536 // GitHub's body limit, in characters
	maxItems = 10    // labels or assignees per list; GitHub allows at most 10 assignees
	maxLabel = 50    // GitHub's label name limit
)

var loginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func ValidKey(key string) bool { return keyPattern.MatchString(key) }

type RepoPermissions struct{ Admin, Maintain, Push, Triage, Pull bool }
type Repo struct {
	HasIssues, Archived bool
	Permissions         RepoPermissions
}
type StatusLabel struct{ Name, Color string }

var StatusLabels = []StatusLabel{{"status:todo", "c5def5"}, {"status:in-progress", "fbca04"}, {"status:review", "0e8a16"}}

func CanChangeStatusLabels(r Repo) bool {
	return writable(r) && (r.Permissions.Admin || r.Permissions.Maintain)
}
func RequestHash(kind string, repoID, number int64, draft []byte) []byte {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s", kind, repoID, number, draft))
	return sum[:]
}

type CreateDraft struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

type Delta struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

type UpdateDraft struct {
	Labels    Delta        `json:"labels"`
	Assignees Delta        `json:"assignees"`
	State     string       `json:"state"`
	Expect    *Expectation `json:"expect,omitempty"`
}

type Expectation struct {
	Status       string   `json:"status"`
	Assignees    []string `json:"assignees"`
	StatusLabels []string `json:"statusLabels"`
}

type IssueView struct {
	Number    int64    `json:"number"`
	URL       string   `json:"url"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
}

// Outcome is what GitHub actually did. Unapplied lists requested changes the confirmed issue does not show, each as
// "add label X", "remove label X", "add assignee X", "remove assignee X" or "state open|closed" (or one fixed
// sentence when a wiped create's labels and assignees cannot be verified); for a status_labels operation, "create
// label X" for each status label the repository still lacks. AllStepsAnswered (updates only) means every
// step got a definite answer (2xx, 4xx, or failed before it was sent), so nothing of the update can still land and a
// recheck may fail it. Once the draft is wiped the stored Outcome is redacted instead, by SQL redacted_write_outcome, to
// {issue: {number, url, state}, unappliedCount, error, redacted: true}; clients must accept both shapes.
type Outcome struct {
	Issue            *IssueView `json:"issue,omitempty"`
	Unapplied        []string   `json:"unapplied,omitempty"`
	Created          []string   `json:"created,omitempty"`
	Error            string     `json:"error,omitempty"`
	AllStepsAnswered bool       `json:"allStepsAnswered,omitempty"`
}

func names(list []string, what string, valid func(string) bool) ([]string, error) {
	if len(list) > maxItems {
		return nil, fmt.Errorf("at most %d %ss", maxItems, what)
	}
	for i, v := range list {
		if !valid(v) {
			return nil, fmt.Errorf("invalid %s %q", what, v)
		}
		if slices.IndexFunc(list[:i], func(o string) bool { return strings.EqualFold(o, v) }) >= 0 {
			return nil, fmt.Errorf("duplicate %s %q", what, v)
		}
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list, nil
}

func validLabel(v string) bool {
	return strings.TrimSpace(v) == v && v != "" && utf8.RuneCountInString(v) <= maxLabel
}
func validLogin(v string) bool { return loginPattern.MatchString(v) }

func (d *CreateDraft) Validate() (err error) {
	d.Title = strings.TrimSpace(d.Title)
	if d.Title == "" || utf8.RuneCountInString(d.Title) > maxTitle {
		return fmt.Errorf("title must be 1 to %d characters", maxTitle)
	}
	if utf8.RuneCountInString(d.Body) > maxBody {
		return fmt.Errorf("body must be at most %d characters", maxBody)
	}
	if d.Labels, err = names(d.Labels, "label", validLabel); err != nil {
		return err
	}
	d.Assignees, err = names(d.Assignees, "assignee", validLogin)
	return err
}

func (d *UpdateDraft) Validate() (err error) {
	for _, l := range []*[]string{&d.Labels.Add, &d.Labels.Remove} {
		if *l, err = names(*l, "label", validLabel); err != nil {
			return err
		}
	}
	for _, l := range []*[]string{&d.Assignees.Add, &d.Assignees.Remove} {
		if *l, err = names(*l, "assignee", validLogin); err != nil {
			return err
		}
	}
	for _, x := range []Delta{d.Labels, d.Assignees} {
		for _, v := range x.Add {
			if slices.ContainsFunc(x.Remove, func(o string) bool { return strings.EqualFold(o, v) }) {
				return fmt.Errorf("%q is both added and removed", v)
			}
		}
	}
	if d.State != "" && d.State != "open" && d.State != "closed" {
		return errors.New(`state must be "open" or "closed"`)
	}
	if len(d.Labels.Add)+len(d.Labels.Remove)+len(d.Assignees.Add)+len(d.Assignees.Remove) == 0 && d.State == "" {
		return errors.New("no change requested")
	}
	if e := d.Expect; e != nil {
		if !slices.Contains(boardStates, e.Status) {
			return fmt.Errorf("expect.status must be one of %s", strings.Join(boardStates, ", "))
		}
		if e.Assignees, err = names(e.Assignees, "expected assignee", validLogin); err != nil {
			return err
		}
		e.StatusLabels, err = names(e.StatusLabels, "expected status label", func(v string) bool { return statusRank(v) > 0 })
		return err
	}
	return nil
}

func (e *Expectation) Matches(issue *IssueView) bool {
	if e == nil {
		return true
	}
	sameSet := func(a, b []string) bool {
		within := func(x, y []string) bool { return !slices.ContainsFunc(x, func(v string) bool { return !has(y, v) }) }
		return within(a, b) && within(b, a)
	}
	if BoardStatus(issue.State, issue.Labels) != e.Status || !sameSet(issue.Assignees, e.Assignees) {
		return false
	}
	live := slices.DeleteFunc(slices.Clone(issue.Labels), func(l string) bool { return statusRank(l) == 0 })
	return issue.State == "closed" || sameSet(live, e.StatusLabels)
}

func ContentHash(title, body string) []byte {
	norm := func(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n") }
	sum := sha256.Sum256([]byte(norm(title) + "\x00" + norm(body)))
	return sum[:]
}

var boardStates = []string{"backlog", "todo", "in_progress", "in_review", "done"}

func statusRank(label string) int {
	for i, l := range StatusLabels {
		if strings.ToLower(label) == l.Name {
			return i + 1
		}
	}
	return 0
}

func BoardStatus(state string, labels []string) string {
	if state == "closed" {
		return boardStates[len(boardStates)-1]
	}
	top := 0
	for _, l := range labels {
		top = max(top, statusRank(l))
	}
	return boardStates[top]
}

// Per-action rules follow GitHub: anyone who can read may open a plain issue; labels or assignees on a new issue need
// push (GitHub silently drops them otherwise); changing labels or assignees later and closing or reopening need
// triage. Archived repositories and repositories without issues accept no writes.
func canPush(r Repo) bool { p := r.Permissions; return p.Push || p.Maintain || p.Admin }
func canTriage(r Repo) bool {
	return r.Permissions.Triage || canPush(r)
}
func writable(r Repo) bool  { return r.HasIssues && !r.Archived }
func CanCreate(r Repo) bool { return writable(r) && (r.Permissions.Pull || canTriage(r)) }
func CanCreateWithMetadata(r Repo) bool {
	return CanCreate(r) && canPush(r)
}
func CanManage(r Repo) bool {
	return writable(r) && canTriage(r)
}

func has(list []string, v string) bool {
	return slices.ContainsFunc(list, func(o string) bool { return strings.EqualFold(o, v) })
}

func Unapplied(issue *IssueView, labels, assignees Delta, state string) []string {
	var out []string
	for _, x := range []struct {
		what    string
		want    Delta
		current []string
	}{{"label", labels, issue.Labels}, {"assignee", assignees, issue.Assignees}} {
		for _, v := range x.want.Add {
			if !has(x.current, v) {
				out = append(out, "add "+x.what+" "+v)
			}
		}
		for _, v := range x.want.Remove {
			if has(x.current, v) {
				out = append(out, "remove "+x.what+" "+v)
			}
		}
	}
	if state != "" && issue.State != state {
		out = append(out, "state "+state)
	}
	return out
}

func Resolved(out Outcome) string {
	if len(out.Unapplied) > 0 {
		return "failed"
	}
	return "succeeded"
}
