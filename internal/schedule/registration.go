package schedule

import "time"

// Registration is when online registration opens for one session, per audience.
// A nil time means that audience has no online registration date; a
// Registration with all three nil means the session is in person only.
type Registration struct {
	Residents *time.Time `json:"residents"`
	Members   *time.Time `json:"members"`
	Public    *time.Time `json:"public"`
}

// Online reports whether any audience can register online.
func (r *Registration) Online() bool {
	return r != nil && (r.Residents != nil || r.Members != nil || r.Public != nil)
}

// RegTime is one audience's opening time, either relative to the session start
// (Before, in wall-clock seconds) or a fixed instant (At). Both nil means no date.
// PerfectMind counts "N hours before" on the local clock, so across a DST change
// the real gap differs by an hour.
type RegTime struct {
	Before *int64     `json:"before,omitempty"`
	At     *time.Time `json:"at,omitempty"`
}

// RegRule is the registration pattern of one PerfectMind course (EventId),
// learned from a single occurrence and applied to all of its occurrences.
type RegRule struct {
	Residents RegTime   `json:"residents"`
	Members   RegTime   `json:"members"`
	Public    RegTime   `json:"public"`
	Fetched   time.Time `json:"fetched"`
}

// relativeLimit separates "N hours before each session" from a season-wide
// fixed date: anything opening more than this long before the sample session
// is treated as fixed.
const relativeLimit = 14 * 24 * time.Hour

// DeriveRegRule turns one occurrence's opening times into a rule.
func DeriveRegRule(residents, members, public *time.Time, start time.Time) RegRule {
	one := func(v *time.Time) RegTime {
		if v == nil {
			return RegTime{}
		}
		d := wall(start).Sub(wall(v.In(start.Location())))
		if d >= 0 && d <= relativeLimit {
			s := int64(d / time.Second)
			return RegTime{Before: &s}
		}
		at := *v
		return RegTime{At: &at}
	}
	return RegRule{Residents: one(residents), Members: one(members), Public: one(public)}
}

// Apply computes the opening times for an occurrence starting at start.
func (r RegRule) Apply(start time.Time) *Registration {
	one := func(t RegTime) *time.Time {
		switch {
		case t.Before != nil:
			w := wall(start).Add(-time.Duration(*t.Before) * time.Second)
			v := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), 0, start.Location())
			return &v
		case t.At != nil:
			v := t.At.In(start.Location())
			return &v
		}
		return nil
	}
	return &Registration{Residents: one(r.Residents), Members: one(r.Members), Public: one(r.Public)}
}

// wall returns t's local clock reading as a UTC instant, so subtraction counts
// clock hours rather than elapsed hours.
func wall(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}
