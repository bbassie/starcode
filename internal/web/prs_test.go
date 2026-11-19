package web

import (
	"slices"
	"testing"

	"starcode/internal/store"
)

// The badge poll leaves out what the watch reads and what cannot change,
// and takes closed PRs and settled threads' only on the slow round.
func TestPollRefs(t *testing.T) {
	pr := func(n int) store.PR { return store.PR{Repo: "o/r", Number: n} }
	th := func(n int, watched, archived bool) store.Thread {
		t := store.Thread{PRRepo: "o/r", PRNumber: n, Archived: archived}
		if watched {
			t.PRWatch = "{}"
		}
		return t
	}
	ts := []store.Thread{
		th(1, false, false), // open, polled
		th(2, true, false),  // watched
		th(2, false, true),  // same PR on a settled thread: still watched
		th(3, false, false), // merged
		th(4, false, false), // closed
		th(5, false, true),  // settled thread only
		th(6, false, false), // never polled
		{},                  // no PR
	}
	before := map[store.PR]store.PRState{
		pr(1): {State: "open"}, pr(2): {State: "open"}, pr(3): {State: "merged"}, pr(4): {State: "closed"}, pr(5): {State: "open"},
	}
	if got := pollRefs(ts, before, false); !slices.Equal(got, []store.PR{pr(1), pr(6)}) {
		t.Errorf("fast round = %v", got)
	}
	if got := pollRefs(ts, before, true); !slices.Equal(got, []store.PR{pr(1), pr(4), pr(5), pr(6)}) {
		t.Errorf("slow round = %v", got)
	}
}
