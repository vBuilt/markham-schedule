// Package schedule holds the core domain types shared across the service.
package schedule

import (
	"strings"
	"time"
)

type Address struct {
	Street     string `json:"street"`
	City       string `json:"city"`
	PostalCode string `json:"postal_code"`
}

type Session struct {
	ID          string    `json:"id"`
	Categories  []string  `json:"categories"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Start       time.Time `json:"start"` // in America/Toronto
	End         time.Time `json:"end"`
	AllDay      bool      `json:"all_day"`
	Centre      string    `json:"centre"`
	Address     Address   `json:"address"`
	Facility    string    `json:"facility"`
	Age         string    `json:"age"`
	MinAge      *int      `json:"min_age"`
	MaxAge      *int      `json:"max_age"`
	Price       string    `json:"price"`
	Spots       string    `json:"spots"`
	SpotsLeft   *int      `json:"spots_left"`
	CourseID    string    `json:"course_id"`
	BookingURL  string    `json:"booking_url"`
	// Registration is nil until the course's registration rule has been looked up.
	Registration *Registration `json:"registration"`
	ScrapedAt    time.Time     `json:"scraped_at"`
}

// Filter selects sessions. Values within a field are ORed, fields are ANDed;
// all string matching is case-insensitive substring matching.
type Filter struct {
	Categories, Centres []string
	// Activities are exact session names (case-insensitive). A session passes
	// the program filter if it is in any of Categories OR is any of Activities,
	// so a whole program and single activities from other programs combine.
	Activities []string
	Query      string    // substring of name or description
	From, To   time.Time // zero = unbounded; session matches if Start >= From and Start < To
	Limit      int       // 0 = no limit
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func (f Filter) Matches(s Session) bool {
	if len(f.Categories) > 0 || len(f.Activities) > 0 {
		ok := false
		for _, want := range f.Categories {
			for _, c := range s.Categories {
				if containsFold(c, want) {
					ok = true
				}
			}
		}
		for _, want := range f.Activities {
			if strings.EqualFold(s.Name, want) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if len(f.Centres) > 0 {
		ok := false
		for _, want := range f.Centres {
			if containsFold(s.Centre, want) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if f.Query != "" && !containsFold(s.Name, f.Query) && !containsFold(s.Description, f.Query) {
		return false
	}
	if !f.From.IsZero() && s.Start.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && !s.Start.Before(f.To) {
		return false
	}
	return true
}

type CategoryResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Count int    `json:"count"`
	Error string `json:"error,omitempty"`
	// DurationMS is how long fetching this category took.
	DurationMS int64 `json:"duration_ms"`
}

// RegStats summarises one refresh's registration-page lookups.
type RegStats struct {
	Courses  int `json:"courses"`   // distinct courses seen this run
	LookedUp int `json:"looked_up"` // pages fetched this run
	Failed   int `json:"failed"`
	Unknown  int `json:"unknown"` // courses still without a rule afterwards
}

// Run is the record of one refresh attempt, kept for the status page.
type Run struct {
	Started      time.Time        `json:"started"`
	Finished     time.Time        `json:"finished"`
	DurationMS   int64            `json:"duration_ms"`
	OK           bool             `json:"ok"`
	Error        string           `json:"error,omitempty"`
	Categories   []CategoryResult `json:"categories"`
	Total        int              `json:"total"`
	Registration RegStats         `json:"registration"`
}

type Status struct {
	LastAttempt *time.Time       `json:"last_attempt"`
	LastSuccess *time.Time       `json:"last_success"`
	Categories  []CategoryResult `json:"categories"`
	Total       int              `json:"total"`
	Error       string           `json:"error,omitempty"`
}
