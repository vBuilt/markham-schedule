package perfectmind

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func toronto(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestToSessionFromFixture(t *testing.T) {
	loc := toronto(t)
	b, err := os.ReadFile("testdata/classes_page1.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp ClassesResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Classes) == 0 {
		t.Fatal("no classes in fixture")
	}
	r := resp.Classes[0]
	now := time.Now()
	s, err := ToSession(r, "Aquafit", "https://example.com/book", loc, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ID) != 16 {
		t.Errorf("ID len = %d", len(s.ID))
	}
	if s.Start.Location() != loc {
		t.Errorf("Start location = %v", s.Start.Location())
	}
	if want := s.Start.Add(time.Duration(r.DurationInMinutes) * time.Minute); !s.End.Equal(want) {
		t.Errorf("End = %v, want %v", s.End, want)
	}
	if s.Centre != r.Address.AddressTag || s.Centre == "" {
		t.Errorf("Centre = %q", s.Centre)
	}
	if len(s.Categories) != 1 || s.Categories[0] != "Aquafit" {
		t.Errorf("Categories = %v", s.Categories)
	}
	if s.BookingURL != "https://example.com/book" || !s.ScrapedAt.Equal(now) {
		t.Errorf("booking/scraped wrong: %q %v", s.BookingURL, s.ScrapedAt)
	}
	if s.Address.Street != r.Address.Street || s.Name != r.EventName {
		t.Errorf("address/name not mapped")
	}
	if r.MinAge != nil && (s.MinAge == nil || *s.MinAge != *r.MinAge) {
		t.Errorf("MinAge not mapped")
	}
}

func TestToSessionAcrossDST(t *testing.T) {
	loc := toronto(t)
	cases := map[string]string{
		"20261101": "2026-11-01T20:05:00-05:00",
		"20261031": "2026-10-31T20:05:00-04:00",
	}
	for date, want := range cases {
		r := RawClass{EventId: "e", OccurrenceDate: date, FormattedStartTime: "08:05 PM", FormattedEndTime: "08:50 PM", DurationInMinutes: 45}
		s, err := ToSession(r, "c", "u", loc, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if got := s.Start.Format(time.RFC3339); got != want {
			t.Errorf("%s: got %s want %s", date, got, want)
		}
	}
}

func TestToSessionEndFallback(t *testing.T) {
	loc := toronto(t)
	r := RawClass{EventId: "e", OccurrenceDate: "20260615", FormattedStartTime: "07:00 PM", FormattedEndTime: "08:15 PM"}
	s, err := ToSession(r, "c", "u", loc, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.End.Sub(s.Start); got != 75*time.Minute {
		t.Errorf("same-day duration = %v", got)
	}
	r = RawClass{EventId: "e", OccurrenceDate: "20260615", FormattedStartTime: "11:30 PM", FormattedEndTime: "12:15 AM"}
	s, err = ToSession(r, "c", "u", loc, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := s.End.Format(time.RFC3339); got != "2026-06-16T00:15:00-04:00" {
		t.Errorf("End = %s", got)
	}
}

func TestToSessionAllDay(t *testing.T) {
	loc := toronto(t)
	r := RawClass{EventId: "e", OccurrenceDate: "20261101", AllDayEvent: true, FormattedStartTime: "12:00 AM", FormattedEndTime: "11:59 PM"}
	s, err := ToSession(r, "c", "u", loc, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.AllDay {
		t.Error("AllDay false")
	}
	if got := s.Start.Format(time.RFC3339); got != "2026-11-01T00:00:00-04:00" {
		t.Errorf("Start = %s", got)
	}
	if got := s.End.Format(time.RFC3339); got != "2026-11-02T00:00:00-05:00" {
		t.Errorf("End = %s", got)
	}
}

func TestSessionIDStable(t *testing.T) {
	base := RawClass{EventId: "e", OccurrenceDate: "20261101", FormattedStartTime: "08:05 PM", Facility: "Pool", Spots: "4 spots left", PriceRange: "$3"}
	id := SessionID(base)
	if id != SessionID(base) {
		t.Error("not deterministic")
	}
	changed := base
	changed.Spots, changed.PriceRange = "1 spot left", "$5"
	if SessionID(changed) != id {
		t.Error("spots/price changed ID")
	}
	changed = base
	changed.FormattedStartTime = "09:05 PM"
	if SessionID(changed) == id {
		t.Error("start time did not change ID")
	}
}

func TestParseSpots(t *testing.T) {
	for in, want := range map[string]int{"4 spots left": 4, "1 spot left": 1} {
		if got := ParseSpots(in); got == nil || *got != want {
			t.Errorf("%q: got %v want %d", in, got, want)
		}
	}
	for _, in := range []string{"", "Full", "Waitlist"} {
		if got := ParseSpots(in); got != nil {
			t.Errorf("%q: got %d want nil", in, *got)
		}
	}
}

func TestToSessionBadDate(t *testing.T) {
	r := RawClass{EventId: "e", OccurrenceDate: "garbage", FormattedStartTime: "08:05 PM"}
	if _, err := ToSession(r, "c", "u", toronto(t), time.Now()); err == nil {
		t.Error("expected error")
	}
}

func TestToSessionNormalisesNameWhitespace(t *testing.T) {
	r := RawClass{EventId: "e", EventName: " Drop-In  Recreational Skate ", OccurrenceDate: "20261101", FormattedStartTime: "08:05 PM", DurationInMinutes: 45}
	s, err := ToSession(r, "Skating", "u", toronto(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "Drop-In Recreational Skate" {
		t.Fatalf("name %q", s.Name)
	}
}
