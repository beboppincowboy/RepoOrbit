package fake

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

func (s *Server) SetWorkflows(id int64, v ...githubapp.Workflow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workflows[id] = append([]githubapp.Workflow{}, v...)
}
func (s *Server) SetWorkflowRuns(id int64, v ...githubapp.WorkflowRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[id] = append([]githubapp.WorkflowRun{}, v...)
}
func (s *Server) SetWorkflowJobs(id, run int64, v ...githubapp.WorkflowJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.workflowJobs[id] == nil {
		s.workflowJobs[id] = map[int64][]githubapp.WorkflowJob{}
	}
	s.workflowJobs[id][run] = append([]githubapp.WorkflowJob{}, v...)
}
func (s *Server) SetDeployments(id int64, v ...githubapp.Deployment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deployments[id] = append([]githubapp.Deployment{}, v...)
}
func (s *Server) SetDeploymentStatuses(id, deployment int64, v ...githubapp.DeploymentStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deploymentStatuses[id] == nil {
		s.deploymentStatuses[id] = map[int64][]githubapp.DeploymentStatus{}
	}
	s.deploymentStatuses[id][deployment] = append([]githubapp.DeploymentStatus{}, v...)
}
func (s *Server) SetDeliveryPermissionDenied(id int64, actions, deployments bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveryDenied[id] = [2]bool{actions, deployments}
}

func (s *Server) SetDeliveryEndpointDenied(id int64, suffix string, denied bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deliveryDeniedPath == nil {
		s.deliveryDeniedPath = map[int64]map[string]bool{}
	}
	if s.deliveryDeniedPath[id] == nil {
		s.deliveryDeniedPath[id] = map[string]bool{}
	}
	s.deliveryDeniedPath[id][suffix] = denied
}

func (s *Server) repoDelivery(w http.ResponseWriter, r *http.Request, login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id int64
	for repoID, x := range s.repos {
		if x.owner == r.PathValue("owner") && x.name == r.PathValue("repo") && s.canRead(login, x) {
			id = repoID
			break
		}
	}
	if id == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	actions := strings.Contains(r.URL.Path, "/actions/")
	denied := s.deliveryDenied[id]
	suffix := strings.TrimPrefix(r.URL.Path, "/repos/"+r.PathValue("owner")+"/"+r.PathValue("repo"))
	if (actions && denied[0]) || (!actions && denied[1]) || s.deliveryDeniedPath[id][suffix] {
		http.Error(w, "permission denied", http.StatusForbidden)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/workflows"):
		deliveryFixturePage(w, r, "workflows", s.workflows[id])
	case strings.HasSuffix(r.URL.Path, "/jobs"):
		run, _ := strconv.ParseInt(r.PathValue("run"), 10, 64)
		deliveryFixturePage(w, r, "jobs", s.workflowJobs[id][run])
	case strings.HasSuffix(r.URL.Path, "/actions/runs"):
		deliveryFixturePage(w, r, "workflow_runs", s.runs[id])
	case strings.HasSuffix(r.URL.Path, "/statuses"):
		deployment, _ := strconv.ParseInt(r.PathValue("deployment"), 10, 64)
		activityFixturePage(w, r, s.deploymentStatuses[id][deployment])
	default:
		activityFixturePage(w, r, s.deployments[id])
	}
}

func deliveryFixturePage[T any](w http.ResponseWriter, r *http.Request, key string, items []T) {
	page := paginateFixture(w, r, items)
	writeJSON(w, map[string]any{"total_count": len(items), key: page})
}
