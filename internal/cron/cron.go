// Package cron reads five-field cron expressions (minute, hour, day of
// month, month, day of week) and works out when one fires next, in a time
// zone of its own. It takes lists, ranges and steps (1,15 9-17 */5),
// month and day names (jan, mon), 7 for Sunday, and the usual shorthands
// (@hourly, @daily, @weekly, @monthly, @yearly). As in Vixie cron, a
// restricted day of month and day of week fire on either.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // the binary is static; a host without zoneinfo still knows the zones
)

// Schedule is a parsed expression.
type Schedule struct {
	min, hour, dom, month, dow uint64
	domStar, dowStar           bool
}

var shorthands = map[string]string{
	"@hourly":   "0 * * * *",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@weekly":   "0 0 * * 0",
	"@monthly":  "0 0 1 * *",
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
}

var monthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
var dayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// Parse reads an expression.
func Parse(expr string) (Schedule, error) {
	expr = strings.TrimSpace(strings.ToLower(expr))
	if s, ok := shorthands[expr]; ok {
		expr = s
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return Schedule{}, fmt.Errorf("cron %q: want five fields (minute hour day month weekday)", expr)
	}
	var s Schedule
	var err error
	if s.min, err = field(f[0], 0, 59, nil); err != nil {
		return s, fmt.Errorf("minute: %w", err)
	}
	if s.hour, err = field(f[1], 0, 23, nil); err != nil {
		return s, fmt.Errorf("hour: %w", err)
	}
	if s.dom, err = field(f[2], 1, 31, nil); err != nil {
		return s, fmt.Errorf("day of month: %w", err)
	}
	if s.month, err = field(f[3], 1, 12, monthNames); err != nil {
		return s, fmt.Errorf("month: %w", err)
	}
	if s.dow, err = field(f[4], 0, 7, dayNames); err != nil {
		return s, fmt.Errorf("day of week: %w", err)
	}
	if s.dow&(1<<7) != 0 {
		s.dow |= 1
	}
	s.domStar, s.dowStar = f[2] == "*" || f[2] == "?", f[4] == "*" || f[4] == "?"
	return s, nil
}

// field reads one field into a bit set of the values it takes.
func field(f string, lo, hi int, names []string) (uint64, error) {
	var set uint64
	for _, part := range strings.Split(f, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("bad step %q", stepStr)
			}
			step = n
		}
		from, to := lo, hi
		switch {
		case rng == "*" || rng == "?":
		default:
			a, b, isRange := strings.Cut(rng, "-")
			var err error
			if from, err = value(a, lo, names); err != nil {
				return 0, err
			}
			to = from
			if isRange {
				if to, err = value(b, lo, names); err != nil {
					return 0, err
				}
			} else if hasStep {
				to = hi
			}
		}
		if from < lo || to > hi || from > to {
			return 0, fmt.Errorf("%q is outside %d-%d", part, lo, hi)
		}
		for v := from; v <= to; v += step {
			set |= 1 << v
		}
	}
	if set == 0 {
		return 0, errors.New("empty")
	}
	return set, nil
}

func value(s string, lo int, names []string) (int, error) {
	for i, n := range names {
		if s == n {
			return i + lo, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}
	return n, nil
}

// Next is the first time after t (to the minute) the schedule fires, in
// loc; zero when there is none within five years (a 30 February).
func (s Schedule) Next(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc).Truncate(time.Minute).Add(time.Minute)
	end := t.AddDate(5, 0, 0)
	for t.Before(end) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
			continue
		}
		if s.min&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

func (s Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dow
	case s.dowStar:
		return dom
	}
	return dom || dow
}
