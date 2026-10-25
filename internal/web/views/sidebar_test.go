package views

import (
	"testing"

	"starcode/internal/domain"
	"starcode/internal/store"
)

// A spawned thread sits under its parent, and a running child brings its
// family up with it.
func TestInboxOrderNestsChildren(t *testing.T) {
	ts := []store.Thread{
		{ID: "a", Status: domain.StatusIdle},
		{ID: "p", Status: domain.StatusIdle},
		{ID: "c1", Parent: "p", Status: domain.StatusRunning},
		{ID: "b", Status: domain.StatusIdle},
		{ID: "c2", Parent: "p", Status: domain.StatusIdle},
		{ID: "g", Parent: "c2", Status: domain.StatusIdle},
	}
	var got []string
	for _, x := range inboxOrder(ts) {
		got = append(got, x.ID)
	}
	want := []string{"p", "c1", "c2", "g", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
