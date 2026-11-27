package ingestion

import (
	"context"
	"testing"
	"time"

	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

type pagedDelivery struct {
	deliveryFetcher
	created time.Time
}

func (f pagedDelivery) FetchWorkflowRuns(context.Context, string, string, string) ([]githubapp.WorkflowRun, string, error) {
	return []githubapp.WorkflowRun{{ID: 1, CreatedAt: f.created}}, "page=2", nil
}
func (f pagedDelivery) FetchWorkflowJobs(context.Context, string, string, int64, string) ([]githubapp.WorkflowJob, string, error) {
	return nil, "", nil
}
func (f pagedDelivery) FetchDeployments(context.Context, string, string, string) ([]githubapp.Deployment, string, error) {
	return []githubapp.Deployment{{ID: 1, CreatedAt: f.created}}, "page=2", nil
}
func (f pagedDelivery) FetchDeploymentStatuses(context.Context, string, string, int64) ([]githubapp.DeploymentStatus, error) {
	return nil, nil
}

func TestDeliveryPagingStopsAtBoundary(t *testing.T) {
	boundary := time.Now()
	for _, stage := range []string{"runs", "deployments"} {
		for created, want := range map[time.Time]string{boundary.Add(time.Hour): "page=2", boundary.Add(-time.Hour): ""} {
			p, err := fetchDeliveryPage(context.Background(), pagedDelivery{created: created}, "", "a/b", stage, "", boundary)
			if err != nil || p.next != want {
				t.Fatalf("%s created=%v next=%q want %q err=%v", stage, created, p.next, want, err)
			}
		}
	}
}
