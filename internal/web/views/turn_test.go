package views

import (
	"strings"
	"testing"
	"time"

	"starcode/internal/domain"
	"starcode/internal/store"
)

// shape spells a turn's fold as kinds: the prompt | the folded rows |
// the rest, with a work block as w and its size.
func shape(items ...store.Item) string {
	turns := GroupTurns(items)
	if len(turns) != 1 {
		return "turns: " + strings.Repeat("x", len(turns))
	}
	head, folded, rest := turns[0].Fold()
	part := func(bs []Block) string {
		var out []string
		for _, b := range bs {
			if b.Item != nil {
				out = append(out, b.Item.Kind)
			} else {
				out = append(out, "w"+strings.Repeat("+", len(b.Work)-1))
			}
		}
		return strings.Join(out, " ")
	}
	return part(head) + " | " + part(folded) + " | " + part(rest)
}

func TestTurnFold(t *testing.T) {
	n := 0
	it := func(kind, status string) store.Item {
		n++
		return store.Item{ID: string(rune('a' + n)), Kind: kind, Status: status}
	}
	user := func() store.Item { return it(domain.KindUser, "") }
	tool := func() store.Item { return it(domain.KindTool, domain.ItemDone) }
	say := func() store.Item { return it(domain.KindAssistant, domain.ItemDone) }
	result := func(st string) store.Item { return it(domain.KindResult, st) }
	for _, c := range []struct {
		name  string
		items []store.Item
		want  string
	}{
		{"narration and work fold", []store.Item{user(), say(), tool(), tool(), say(), result(domain.ItemDone)}, "user | assistant w+ | assistant result"},
		{"one block stays as it is", []store.Item{user(), tool(), tool(), say(), result(domain.ItemDone)}, "user |  | w+ assistant result"},
		{"work after the reply folds too", []store.Item{user(), tool(), say(), tool(), say(), tool(), result(domain.ItemDone)}, "user | w assistant w w | assistant result"},
		{"an error after the reply stays", []store.Item{user(), tool(), say(), tool(), say(), it(domain.KindError, ""), result(domain.ItemDone)}, "user | w assistant w | assistant error result"},
		{"a stopped turn is whole", []store.Item{user(), say(), tool(), say(), result("interrupted")}, "user |  | assistant w assistant result"},
		{"a running turn is whole", []store.Item{user(), say(), tool(), say()}, "user |  | assistant w assistant"},
		{"no reply, nothing to fold to", []store.Item{user(), tool(), result(domain.ItemDone)}, "user |  | w result"},
	} {
		if got := shape(c.items...); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

func TestNewMark(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	n := 0
	it := func(kind, status string, min int) store.Item {
		n++
		return store.Item{ID: string(rune('a' + n)), Kind: kind, Status: status, CreatedAt: t0.Add(time.Duration(min) * time.Minute)}
	}
	done := domain.ItemDone
	// A finished turn: prompt b, fold of c..e, reply f, result g.
	turn1 := []store.Item{it(domain.KindUser, "", 0), it(domain.KindAssistant, done, 1), it(domain.KindTool, done, 2), it(domain.KindTool, done, 3), it(domain.KindAssistant, done, 4), it(domain.KindResult, done, 5)}
	// A second one, still running: prompt h, tool i.
	turn2 := []store.Item{it(domain.KindUser, "", 10), it(domain.KindTool, done, 11)}
	items := append(append([]store.Item{}, turn1...), turn2...)
	for _, c := range []struct {
		name  string
		since time.Time
		want  string
	}{
		{"never looked", time.Time{}, ""},
		{"nothing new", t0.Add(20 * time.Minute), ""},
		{"a new prompt", t0.Add(6 * time.Minute), "it-" + turn2[0].ID},
		{"new work inside a fold marks the fold", t0.Add(150 * time.Second), "fold-" + turn1[0].ID},
		{"only the running turn's tool", t0.Add(10*time.Minute + 30*time.Second), "work-" + turn2[1].ID},
		{"everything is new", t0.Add(-time.Minute), ""},
	} {
		if got := NewMark(items, c.since); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
