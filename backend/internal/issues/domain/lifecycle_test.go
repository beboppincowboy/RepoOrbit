package domain

import "testing"

func TestLifecycleTransitions(t *testing.T) {
	for _, tc := range []struct {
		from, to State
		allowed  bool
	}{
		{"", Pending, true}, {Pending, Unknown, true}, {Pending, Failed, true},
		{Unknown, Succeeded, true}, {Unknown, Failed, true}, {Unknown, Unknown, true},
		{Succeeded, Unknown, false}, {Failed, Pending, false}, {Pending, Succeeded, false},
	} {
		err := Transition(tc.from, tc.to)
		if (err == nil) != tc.allowed {
			t.Errorf("%q -> %q: %v", tc.from, tc.to, err)
		}
	}
}
