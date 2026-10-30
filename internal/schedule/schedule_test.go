package schedule

import (
	"testing"
	"time"
)

func TestFilterActivities(t *testing.T) {
	lane := Session{Name: "Drop-In Lane Swim", Categories: []string{"Swimming"}, Centre: "Cornell Community Centre"}
	rec := Session{Name: "Drop-In Recreational Swim", Categories: []string{"Swimming"}, Centre: "Cornell Community Centre"}
	aqua := Session{Name: "Drop-In Aquafit: Shallow/Deep", Categories: []string{"Aquafit"}, Centre: "Angus Glen Community Centre"}
	skate := Session{Name: "Drop-In Public Skate", Categories: []string{"Skating"}, Centre: "Cornell Community Centre"}

	cases := []struct {
		name string
		f    Filter
		want []bool // lane, rec, aqua, skate
	}{
		{"activity only", Filter{Activities: []string{"drop-in lane swim"}}, []bool{true, false, false, false}},
		{"activity is exact, not substring", Filter{Activities: []string{"Lane"}}, []bool{false, false, false, false}},
		{"whole program plus one activity from another", Filter{Categories: []string{"Aquafit"}, Activities: []string{"Drop-In Lane Swim"}}, []bool{true, false, true, false}},
		{"centre still narrows", Filter{Activities: []string{"Drop-In Lane Swim", "Drop-In Aquafit: Shallow/Deep"}, Centres: []string{"cornell"}}, []bool{true, false, false, false}},
		{"no program filters", Filter{}, []bool{true, true, true, true}},
	}
	for _, c := range cases {
		for i, s := range []Session{lane, rec, aqua, skate} {
			if got := c.f.Matches(s); got != c.want[i] {
				t.Errorf("%s: session %d (%s) = %v, want %v", c.name, i, s.Name, got, c.want[i])
			}
		}
	}
}

func TestRegRuleDeriveAndApply(t *testing.T) {
	loc, _ := time.LoadLocation("America/Toronto")
	at := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, loc) }
	start := at(1, 19, 15)
	res, mem := at(0, 22, 15), at(1, 16, 15) // Oct 31 22:15 (21h before), Nov 1 16:15 (3h before)
	season := time.Date(2026, 9, 6, 6, 30, 0, 0, loc)

	r := DeriveRegRule(&res, &mem, &mem, start)
	if r.Residents.Before == nil || *r.Residents.Before != 21*3600 || r.Members.Before == nil || *r.Members.Before != 3*3600 {
		t.Fatalf("relative rule %+v", r)
	}
	// The same course a week later: dates move with the session.
	got := r.Apply(start.AddDate(0, 0, 7))
	if !got.Residents.Equal(res.AddDate(0, 0, 7)) || !got.Public.Equal(mem.AddDate(0, 0, 7)) || !got.Online() {
		t.Fatalf("applied %+v", got)
	}

	// A season-wide date (more than 14 days before) stays fixed.
	abs := DeriveRegRule(&season, nil, nil, start)
	if abs.Residents.At == nil || abs.Residents.Before != nil {
		t.Fatalf("absolute rule %+v", abs)
	}
	if a := abs.Apply(start.AddDate(0, 0, 7)); !a.Residents.Equal(season) || a.Members != nil {
		t.Fatalf("applied absolute %+v", a)
	}

	// No dates at all: in person only.
	none := DeriveRegRule(nil, nil, nil, start).Apply(start)
	if none == nil || none.Online() {
		t.Fatalf("no-registration rule should apply as non-nil, offline: %+v", none)
	}
}
