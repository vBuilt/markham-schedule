package schedule

import "testing"

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
