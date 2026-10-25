package web

import (
	"maps"
	"net/url"
	"testing"

	"starcode/internal/domain"
)

func TestFormAnswers(t *testing.T) {
	qs := []domain.Question{
		{Question: "One?"},
		{Question: "Many?", MultiSelect: true},
		{Question: "Unanswered?"},
	}
	form := url.Values{
		"q0":       {"A"},
		"q0-other": {" my own "},
		"q1":       {"X", "Y"},
		"q1-other": {"Z"},
		"q2-other": {"  "},
	}
	want := map[string]string{"One?": "my own", "Many?": "X, Y, Z"}
	if got := formAnswers(qs, form); !maps.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
