package cron

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 23, 14, 7, 30, 0, ams) // a Friday
	for _, c := range []struct{ expr, want string }{
		{"*/15 * * * *", "2026-10-23 14:15"},
		{"0 9 * * mon-fri", "2026-10-26 09:00"},
		{"30 8 1 * *", "2026-11-01 08:30"},
		{"0 0 * * 7", "2026-10-25 00:00"},
		{"@daily", "2026-10-24 00:00"},
		{"0 9 13 * fri", "2026-10-30 09:00"}, // either the 13th or a Friday
		{"0 12 * jan *", "2027-01-01 12:00"},
		{"5 14 23 10 *", "2027-10-23 14:05"},
	} {
		s, err := Parse(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := s.Next(from, ams).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("%s: got %s, want %s", c.expr, got, c.want)
		}
	}
	// Across the switch to winter time (25 Oct 2026, 03:00 -> 02:00).
	s, _ := Parse("30 2 * * *")
	if got := s.Next(time.Date(2026, 10, 24, 23, 0, 0, 0, ams), ams); got.Day() != 25 || got.Hour() != 2 {
		t.Errorf("dst: %v", got)
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* * 0 * *", "*/0 * * * *", "a b c d e", "5-1 * * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if s, _ := Parse("0 0 30 2 *"); !s.Next(from, ams).IsZero() {
		t.Error("30 February fired")
	}
}
