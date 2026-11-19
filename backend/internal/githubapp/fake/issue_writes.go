package fake

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// IssueWriteFault changes how the next issue creations behave until it is replaced. The zero value is GitHub's
// normal behavior.
type IssueWriteFault struct {
	DropAssignees bool          // answer 201 but silently leave every requested assignee off the issue
	Delay         time.Duration // create the issue, then wait this long before answering
	Status        int           // answer with this status instead of 201
	Applied       bool          // with Status: create the issue anyway, as when a gateway fails after GitHub committed
	Before        func()        // runs when the create request arrives, before anything is stored
}

// maxAssignees is GitHub's per-issue cap; assignees beyond it are silently ignored.
const maxAssignees = 10

var levels = []string{"read", "triage", "write", "maintain", "admin"}

func atLeast(level, min string) bool {
	return slices.Index(levels, level) >= slices.Index(levels, min) && level != ""
}

// SetPermission grants login a level on a repository ("" removes it). The owner is always admin. A grant never makes a
// private repository readable: the fake models private reads as owner-only.
func (s *Server) SetPermission(login string, repoID int64, level string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perms[repoID] == nil {
		s.perms[repoID] = map[string]string{}
	}
	s.perms[repoID][login] = level
}

// SetIssueWriteFault replaces the fault applied to issue creation.
func (s *Server) SetIssueWriteFault(f IssueWriteFault) { s.mu.Lock(); s.writeFault = f; s.mu.Unlock() }

// Issues returns a copy of a repository's issues, including those created through the API.
func (s *Server) Issues(repoID int64) []IssueFixture {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]IssueFixture(nil), s.issues[repoID]...)
}

// WriteTokens lists the bearer tokens that issue write requests carried, in order.
func (s *Server) WriteTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writeTokens...)
}

// level is login's permission on r; "" means no access. Caller holds mu.
func (s *Server) level(login string, r *repo) string {
	switch {
	case login == "":
		return ""
	case r.owner == login:
		return "admin"
	case !s.canRead(login, r):
		return ""
	case s.perms[r.id][login] != "":
		return s.perms[r.id][login]
	}
	return "read"
}

func issueJSON(r *http.Request, issue IssueFixture) map[string]any {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := fmt.Sprintf("%s://%s/repos/%s/%s", scheme, r.Host, r.PathValue("owner"), r.PathValue("repo"))
	entry := map[string]any{"id": issue.ID, "number": issue.Number, "title": issue.Title, "body": issue.Body, "state": issue.State,
		"html_url": issue.HTMLURL, "repository_url": base, "created_at": issue.CreatedAt, "updated_at": issue.UpdatedAt, "closed_at": issue.ClosedAt,
		"user": issue.User, "labels": issue.Labels, "assignees": issue.Assignees}
	if entry["labels"] == nil {
		entry["labels"] = []IssueLabel{}
	}
	if entry["assignees"] == nil {
		entry["assignees"] = []IssueActor{}
	}
	if issue.PullRequest {
		entry["pull_request"] = map[string]string{"url": fmt.Sprintf("%s/pulls/%d", base, issue.Number)}
	}
	return entry
}

// writeTarget resolves the repository of an issue write and the caller's level, recording the bearer token. It writes
// 404 and returns ok=false when login cannot read the repository. Caller holds mu.
func (s *Server) writeTarget(w http.ResponseWriter, r *http.Request, login string) (x *repo, level string, ok bool) {
	if r.Method != http.MethodGet {
		s.writeTokens = append(s.writeTokens, r.Header.Get("Authorization")[len("Bearer "):])
	}
	for _, candidate := range s.repos {
		if candidate.owner == r.PathValue("owner") && candidate.name == r.PathValue("repo") && s.canRead(login, candidate) {
			return candidate, s.level(login, candidate), true
		}
	}
	http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	return nil, "", false
}

// issue finds the addressed issue, or writes 404. Caller holds mu.
func (s *Server) issue(w http.ResponseWriter, r *http.Request, x *repo) *IssueFixture {
	n, _ := strconv.ParseInt(r.PathValue("number"), 10, 64)
	for i := range s.issues[x.id] {
		if s.issues[x.id][i].Number == n {
			return &s.issues[x.id][i]
		}
	}
	http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	return nil
}

func (s *Server) assignableTo(x *repo, login string) bool {
	return x.owner == login || atLeast(s.perms[x.id][login], "triage")
}

func actor(login string) IssueActor {
	return IssueActor{ID: int64(idFor(login)), Login: login, Type: "User"}
}

func labelFor(name string) IssueLabel {
	sum := sha256.Sum256([]byte(name))
	return IssueLabel{ID: int64(binary.BigEndian.Uint32(sum[:4])) + 1, Name: name}
}

// touch advances updated_at strictly, so every change is newer than the last one a client saw.
func touch(issue *IssueFixture) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	if !now.After(issue.UpdatedAt) {
		now = issue.UpdatedAt.Add(time.Microsecond)
	}
	issue.UpdatedAt = now
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		http.Error(w, `{"message":"Problems parsing JSON"}`, http.StatusBadRequest)
		return false
	}
	return true
}

// createIssue mirrors POST /repos/{owner}/{repo}/issues: labels and assignees from a caller without push, and
// assignees who cannot be assigned, are silently dropped, as GitHub does.
func (s *Server) createIssue(w http.ResponseWriter, r *http.Request, login string) {
	var in struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Labels    []string `json:"labels"`
		Assignees []string `json:"assignees"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	fault := s.writeFault
	s.mu.Unlock()
	if fault.Before != nil {
		fault.Before()
	}
	s.mu.Lock()
	x, level, ok := s.writeTarget(w, r, login)
	if !ok {
		s.mu.Unlock()
		return
	}
	if in.Title == "" {
		s.mu.Unlock()
		http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
		return
	}
	if fault.Status != 0 && !fault.Applied {
		s.mu.Unlock()
		http.Error(w, `{"message":"fault"}`, fault.Status)
		return
	}
	number := int64(1)
	for _, existing := range s.issues[x.id] {
		number = max(number, existing.Number+1)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	issue := IssueFixture{ID: 700000 + x.id*1000 + number, Number: number, Title: in.Title, Body: in.Body, State: "open",
		HTMLURL: fmt.Sprintf("https://github.com/%s/%s/issues/%d", x.owner, x.name, number), CreatedAt: now, UpdatedAt: now, User: actor(login)}
	if atLeast(level, "write") {
		for _, name := range in.Labels {
			issue.Labels = append(issue.Labels, labelFor(name))
		}
		for _, a := range in.Assignees {
			if !fault.DropAssignees && s.assignableTo(x, a) && len(issue.Assignees) < maxAssignees {
				issue.Assignees = append(issue.Assignees, actor(a))
			}
		}
	}
	s.issues[x.id] = append(s.issues[x.id], issue)
	s.mu.Unlock()
	time.Sleep(fault.Delay)
	if fault.Status != 0 {
		http.Error(w, `{"message":"fault"}`, fault.Status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(issueJSON(r, issue))
}

// issueByNumber serves GET and PATCH (state only) on one issue.
func (s *Server) issueByNumber(w http.ResponseWriter, r *http.Request, login string) {
	var in struct {
		State string `json:"state"`
	}
	if r.Method == http.MethodPatch && !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	x, level, ok := s.writeTarget(w, r, login)
	if !ok {
		return
	}
	issue := s.issue(w, r, x)
	if issue == nil {
		return
	}
	if r.Method == http.MethodPatch {
		if !atLeast(level, "triage") && issue.User.Login != login {
			http.Error(w, `{"message":"Must have admin rights to Repository."}`, http.StatusForbidden)
			return
		}
		if in.State != "open" && in.State != "closed" {
			http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
			return
		}
		issue.State = in.State
		issue.ClosedAt = nil
		if in.State == "closed" {
			now := time.Now().UTC()
			issue.ClosedAt = &now
		}
		touch(issue)
	}
	writeJSON(w, issueJSON(r, *issue))
}

// issueLabels serves POST (add) on the labels collection and DELETE on one label.
func (s *Server) issueLabels(w http.ResponseWriter, r *http.Request, login string) {
	var in struct {
		Labels []string `json:"labels"`
	}
	if r.Method == http.MethodPost && !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	x, level, ok := s.writeTarget(w, r, login)
	if !ok {
		return
	}
	issue := s.issue(w, r, x)
	if issue == nil {
		return
	}
	if !atLeast(level, "triage") {
		http.Error(w, `{"message":"Must have triage rights to Repository."}`, http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPost:
		for _, name := range in.Labels {
			if !slices.ContainsFunc(issue.Labels, func(l IssueLabel) bool { return l.Name == name }) {
				issue.Labels = append(issue.Labels, labelFor(name))
			}
		}
	case http.MethodDelete:
		i := slices.IndexFunc(issue.Labels, func(l IssueLabel) bool { return l.Name == r.PathValue("name") })
		if i < 0 {
			http.Error(w, `{"message":"Label does not exist"}`, http.StatusNotFound)
			return
		}
		issue.Labels = slices.Delete(issue.Labels, i, i+1)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	touch(issue)
	writeJSON(w, issue.Labels)
}

// issueAssignees serves POST (add) and DELETE (remove) of assignees. Unassignable logins, and additions beyond
// maxAssignees, are silently ignored.
func (s *Server) issueAssignees(w http.ResponseWriter, r *http.Request, login string) {
	var in struct {
		Assignees []string `json:"assignees"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	x, level, ok := s.writeTarget(w, r, login)
	if !ok {
		return
	}
	issue := s.issue(w, r, x)
	if issue == nil {
		return
	}
	if !atLeast(level, "triage") {
		http.Error(w, `{"message":"Must have triage rights to Repository."}`, http.StatusForbidden)
		return
	}
	status := http.StatusOK
	for _, a := range in.Assignees {
		i := slices.IndexFunc(issue.Assignees, func(u IssueActor) bool { return u.Login == a })
		switch {
		case r.Method == http.MethodPost && i < 0 && s.assignableTo(x, a) && len(issue.Assignees) < maxAssignees:
			issue.Assignees = append(issue.Assignees, actor(a))
		case r.Method == http.MethodDelete && i >= 0:
			issue.Assignees = slices.Delete(issue.Assignees, i, i+1)
		}
	}
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	touch(issue)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(issueJSON(r, *issue))
}

// assignable mirrors GET /repos/{owner}/{repo}/assignees/{login}: 204 when the login can be assigned, else 404.
func (s *Server) assignable(w http.ResponseWriter, r *http.Request, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, _, ok := s.writeTarget(w, r, login)
	if !ok {
		return
	}
	if !s.assignableTo(x, r.PathValue("login")) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
