package issues

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"
)

// Pins today's request and content hashes and board statuses: stored idempotency keys must keep replaying.
func TestWriteRulesGolden(t *testing.T) {
	drafts := []any{
		&createDraft{Title: "  A title  ", Body: "Body\r\ntext", Labels: []string{"bug", "help"}, Assignees: []string{"Ada"}},
		&updateDraft{Labels: delta{Add: []string{"bug"}}},
		&updateDraft{Labels: delta{Remove: []string{"bug"}}, Expect: &expectation{Status: "todo", StatusLabels: []string{"status:todo"}}},
		&updateDraft{Assignees: delta{Add: []string{"Ada"}, Remove: []string{"Bob"}}},
		&updateDraft{State: "closed", Expect: &expectation{Status: "backlog", Assignees: []string{"Ada"}}},
	}
	wants := [][2]string{{"022548d7556f029df263cb5bd035040314721309e5256e84643c7f401546613a", "6bac4f40f47230fb55390fc5e03726864f0cb075c94c1fe043facc3cdcef0ed4"}, {"8c2a5e21676af512c1942ef7b7aaf9ec0cef5a768050023405d3ee81fdfaa4b4", "1ca83fb9817dd72e0e382f4ddf8dfb5a1cd90520d912510320bf4edd4a13d339"}, {"a261d240e20b5455cc0f0a5d11a574ab9405a4366c6cdc3733b9f077322ccf9e", "42aa91d373051523123d15266ac91fdab5006d82b82b0f45743c49cf7da7317a"}, {"d9403020570d2144216ebb4a08d8cd893294887d879a34a5769625d13c068023", "012641304760e5340b062ae1d301d365d05824687a1c583b7ef8bfc46f914871"}, {"81d25d12a5a70e46ceec0fca5c01cd6f22d29e43e64300f369b2b42fe687bd7e", "435a71ab365aa06729cf0345897640e4a198695bc1b9ee9b09faab6eed556602"}}
	for i, d := range drafts {
		switch v := d.(type) {
		case *createDraft:
			if err := v.validate(); err != nil {
				t.Fatal(err)
			}
		case *updateDraft:
			if err := v.validate(); err != nil {
				t.Fatal(err)
			}
		}
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		kind, number := "update", int64(7)
		if i == 0 {
			kind, number = "create", 0
		}
		// The request hash as submit computes it.
		sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s", kind, int64(42), number, b))
		if got := fmt.Sprintf("%x", sum[:]); got != wants[i][0] {
			t.Errorf("%d request %s", i, got)
		}
		if got := fmt.Sprintf("%x", contentHash(string(b), " body\r\n")); got != wants[i][1] {
			t.Errorf("%d content %s", i, got)
		}
	}
	for _, tc := range []struct {
		state  string
		labels []string
		want   string
	}{{"closed", []string{"status:todo"}, "done"}, {"open", []string{"status:todo", "STATUS:REVIEW"}, "in_review"}, {"open", []string{"Status:In-Progress"}, "in_progress"}, {"open", []string{"bug"}, "backlog"}} {
		if got := boardStatus(tc.state, tc.labels); got != tc.want {
			t.Errorf("board %v = %q want %q", tc.labels, got, tc.want)
		}
	}
}
