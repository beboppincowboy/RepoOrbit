package fake

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type PullRequestFixture struct {
	ID        int64      `json:"id"`
	Number    int64      `json:"number"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	MergedAt  *time.Time `json:"merged_at"`
	User      IssueActor `json:"user"`
}
type ReviewFixture struct {
	ID          int64      `json:"id"`
	User        IssueActor `json:"user"`
	State       string     `json:"state"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	SubmittedAt *time.Time `json:"submitted_at"`
}
type CommentFixture struct {
	ID        int64      `json:"id"`
	User      IssueActor `json:"user"`
	Body      string     `json:"body"`
	HTMLURL   string     `json:"html_url"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type ReleaseFixture struct {
	ID          int64       `json:"id"`
	Author      *IssueActor `json:"author"`
	Name        string      `json:"name"`
	TagName     string      `json:"tag_name"`
	Body        string      `json:"body"`
	HTMLURL     string      `json:"html_url"`
	PublishedAt *time.Time  `json:"published_at"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	Draft       bool        `json:"draft"`
	Prerelease  bool        `json:"prerelease"`
}
type EventIssueFixture struct {
	ID     int64 `json:"id"`
	Number int64 `json:"number"`
}
type EventLabelFixture struct {
	Name string `json:"name"`
}
type IssueEventFixture struct {
	ID        int64              `json:"id"`
	Actor     *IssueActor        `json:"actor"`
	Event     string             `json:"event"`
	CreatedAt time.Time          `json:"created_at"`
	URL       string             `json:"url"`
	Issue     *EventIssueFixture `json:"issue"`
	Assignee  *IssueActor        `json:"assignee"`
	Label     *EventLabelFixture `json:"label"`
}

func (s *Server) SetReleases(repoID int64, items ...ReleaseFixture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases[repoID] = append([]ReleaseFixture(nil), items...)
}
func (s *Server) SetIssueEvents(repoID int64, items ...IssueEventFixture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issueEvents[repoID] = append([]IssueEventFixture(nil), items...)
}

func (s *Server) SetPullRequests(repoID int64, items ...PullRequestFixture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pulls[repoID] = append([]PullRequestFixture(nil), items...)
}
func (s *Server) SetReviews(repoID, number int64, items ...ReviewFixture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reviews[repoID] == nil {
		s.reviews[repoID] = map[int64][]ReviewFixture{}
	}
	s.reviews[repoID][number] = append([]ReviewFixture(nil), items...)
}
func (s *Server) SetComments(repoID, number int64, items ...CommentFixture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.comments[repoID] == nil {
		s.comments[repoID] = map[int64][]CommentFixture{}
	}
	s.comments[repoID][number] = append([]CommentFixture(nil), items...)
}

func (s *Server) repoActivity(w http.ResponseWriter, r *http.Request, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id int64
	var found bool
	for repoID, x := range s.repos {
		if x.owner == r.PathValue("owner") && x.name == r.PathValue("repo") {
			id, found = repoID, s.canRead(login, x)
			break
		}
	}
	if !found {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	number, _ := strconv.ParseInt(r.PathValue("number"), 10, 64)
	if strings.HasSuffix(r.URL.Path, "/releases") {
		activityFixturePage(w, r, s.releases[id], false)
	} else if strings.HasSuffix(r.URL.Path, "/issues/events") {
		activityFixturePage(w, r, s.issueEvents[id], false)
	} else if strings.HasSuffix(r.URL.Path, "/pulls") {
		if r.URL.Query().Get("state") != "all" {
			http.Error(w, `{"message":"Bad Request"}`, http.StatusBadRequest)
			return
		}
		activityFixturePage(w, r, s.pulls[id], true)
	} else if number <= 0 {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	} else if strings.HasSuffix(r.URL.Path, "/reviews") {
		activityFixturePage(w, r, s.reviews[id][number], false)
	} else {
		activityFixturePage(w, r, s.comments[id][number], false)
	}
}

func activityFixturePage[T any](w http.ResponseWriter, r *http.Request, items []T, all bool) {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if per < 1 || per > 100 {
		per = 30
	}
	if pg < 1 {
		pg = 1
	}
	lo, hi := min((pg-1)*per, len(items)), min(pg*per, len(items))
	if hi < len(items) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		state := ""
		if all {
			state = "state=all&"
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s://%s%s?%sper_page=%d&page=%d>; rel="next"`, scheme, r.Host, r.URL.Path, state, per, pg+1))
	}
	out := make([]T, hi-lo)
	copy(out, items[lo:hi])
	writeJSON(w, out)
}
