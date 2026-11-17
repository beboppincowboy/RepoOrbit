package graph

import "testing"

func TestPageLimit(t *testing.T) {
	for _, n := range []int{0, -1, 101} {
		if _, err := pageLimit(n); err == nil {
			t.Fatalf("accepted %d", n)
		}
	}
	if n, err := pageLimit(100); err != nil || n != 100 {
		t.Fatalf("valid page: %d %v", n, err)
	}
}
