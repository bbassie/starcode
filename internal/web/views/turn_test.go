package views

import (
	"strings"
	"testing"

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
