package issues

import (
	"testing"

	"github.com/repoorbit/repoorbit/backend/internal/accessfence"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
)

// A fence or confirmed issue of the wrong type is a wiring bug: it must fail the write, not panic or settle a create
// without linking it. A nil confirmed issue is the normal "nothing confirmed" case.
func TestAdapterUnwrapsOpaqueValues(t *testing.T) {
	if _, err := snapshotOf(accessfence.Snapshot{}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, fence := range []any{nil, &accessfence.Snapshot{}, "fence"} {
		if _, err := snapshotOf(fence); err == nil {
			t.Fatalf("fence %#v: want an error", fence)
		}
	}
	if issue, ok, err := confirmedIssue(githubapp.Issue{Number: 7}); err != nil || !ok || issue.Number != 7 {
		t.Fatalf("issue: got %v, %v, %v", issue, ok, err)
	}
	if _, ok, err := confirmedIssue(nil); err != nil || ok {
		t.Fatalf("nil: got %v, %v", ok, err)
	}
	if _, ok, err := confirmedIssue(&githubapp.Issue{Number: 7}); err == nil || ok {
		t.Fatalf("pointer: got %v, %v; want an error", ok, err)
	}
}
