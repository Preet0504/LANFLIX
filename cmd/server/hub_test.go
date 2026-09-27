package main

import "testing"

// idAfter decides which live chunks a viewer already got during backfill.
// Stream IDs are "<pts ms>-<seq>" and must compare numerically, not as
// strings: "10000-6" comes after "8000-5" although it sorts before it.
func TestIDAfter(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"10000-6", "8000-5", true}, // string order would say false
		{"8000-5", "10000-6", false},
		{"8000-5", "8000-5", false}, // the same chunk is not "after" itself
		{"8000-6", "8000-5", true},  // same ms, higher seq
		{"0-1", "0-0", true},        // first chunk vs "nothing yet"
		{"2000-2", "0-0", true},
	}
	for _, c := range cases {
		if got := idAfter(c.a, c.b); got != c.want {
			t.Errorf("idAfter(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
