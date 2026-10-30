package ics

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/schedule"
)

var toronto, _ = time.LoadLocation("America/Toronto")

func sess(id string) schedule.Session {
	return schedule.Session{
		ID:         id,
		Categories: []string{"Aquafit", "Swim"},
		Name:       "Drop-In Aquafit: Shallow/Deep",
		Start:      time.Date(2026, 11, 1, 20, 5, 0, 0, toronto),
		End:        time.Date(2026, 11, 1, 21, 5, 0, 0, toronto),
		Centre:     "Cornell Community Centre",
		Facility:   "Pool",
		Address:    schedule.Address{Street: "3201 Bur Oak Ave", City: "Markham"},
		Spots:      "4 spots left",
		BookingURL: "https://example.com/book?a=1,2",
		ScrapedAt:  time.Date(2026, 10, 29, 15, 4, 0, 0, toronto),
	}
}

func render(t *testing.T, ss []schedule.Session) string {
	t.Helper()
	var b bytes.Buffer
	err := Write(&b, ss, Options{Name: "Markham", Refresh: 3 * time.Hour, Now: time.Date(2026, 10, 29, 19, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func unfold(s string) string { return strings.ReplaceAll(s, "\r\n ", "") }

func TestWriteBasicEvent(t *testing.T) {
	out := render(t, []schedule.Session{sess("abc")})
	for _, want := range []string{
		"UID:abc@markham-schedule\r\n",
		"DTSTART;TZID=America/Toronto:20261101T200500\r\n",
		"DTEND;TZID=America/Toronto:20261101T210500\r\n",
		"DTSTAMP:20261029T190000Z\r\n",
		"SUMMARY:Drop-In Aquafit: Shallow/Deep\r\n",
		"REFRESH-INTERVAL;VALUE=DURATION:PT3H\r\n",
		"X-PUBLISHED-TTL:PT3H\r\n",
		"LOCATION:Cornell Community Centre – Pool\\, 3201 Bur Oak Ave\\, Markham\r\n",
		"Spots: 4 spots left (as of Oct 29\\, 3:04 PM)",
		"URL:https://example.com/book?a=1,2\r\n",
		"TZID:America/Toronto\r\n",
	} {
		if !strings.Contains(unfold(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "\n") != strings.Count(out, "\r\n") {
		t.Error("bare LF found")
	}
}

func TestEscaping(t *testing.T) {
	s := sess("e")
	s.Name = "a, b; c\nd\\e"
	out := render(t, []schedule.Session{s})
	if !strings.Contains(out, "SUMMARY:a\\, b\\; c\\nd\\\\e\r\n") {
		t.Errorf("bad escaping:\n%s", out)
	}
}

func TestFolding(t *testing.T) {
	s := sess("f")
	s.Name = strings.Repeat("–é", 80) + " end"
	out := render(t, []schedule.Session{s})
	for _, l := range strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n") {
		if len(l) > 75 {
			t.Errorf("line %d octets: %q", len(l), l)
		}
	}
	if !strings.Contains(unfold(out), "SUMMARY:"+s.Name+"\r\n") {
		t.Error("unfold did not restore original")
	}
	if !strings.Contains(out, "\r\n ") {
		t.Error("expected folding")
	}
}

func TestAllDay(t *testing.T) {
	s := sess("d")
	s.AllDay = true
	s.Start = time.Date(2026, 10, 29, 0, 0, 0, 0, toronto)
	s.End = s.Start
	out := render(t, []schedule.Session{s})
	if !strings.Contains(out, "DTSTART;VALUE=DATE:20261029\r\n") || !strings.Contains(out, "DTEND;VALUE=DATE:20261030\r\n") {
		t.Errorf("bad all-day:\n%s", out)
	}
}

func TestNoDuplicateUIDs(t *testing.T) {
	out := render(t, []schedule.Session{sess("x"), sess("x"), sess("y")})
	if n := strings.Count(out, "BEGIN:VEVENT"); n != 2 {
		t.Errorf("got %d events", n)
	}
}

func TestEmptyCalendarValid(t *testing.T) {
	out := render(t, nil)
	if !strings.HasPrefix(out, "BEGIN:VCALENDAR\r\n") || !strings.HasSuffix(out, "END:VCALENDAR\r\n") ||
		!strings.Contains(out, "BEGIN:VTIMEZONE") || strings.Contains(out, "VEVENT") {
		t.Errorf("bad empty calendar:\n%s", out)
	}
}

func TestRegistrationInDescription(t *testing.T) {
	s := sess("r1")
	res := time.Date(2026, 10, 31, 23, 5, 0, 0, toronto)
	mem := time.Date(2026, 11, 1, 17, 5, 0, 0, toronto)
	s.Registration = &schedule.Registration{Residents: &res, Members: &mem, Public: &mem}
	out := unfold(render(t, []schedule.Session{s}))
	for _, want := range []string{
		`Residents register from: Sat Oct 31\, 11:05 PM`,
		`Members register from: Sun Nov 1\, 5:05 PM`,
		`Public register from: Sun Nov 1\, 5:05 PM`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}

	s.Registration = &schedule.Registration{}
	if out := unfold(render(t, []schedule.Session{s})); !strings.Contains(out, "Registration: in person only") {
		t.Fatalf("in-person line missing:\n%s", out)
	}
	s.Registration = nil
	if out := render(t, []schedule.Session{s}); strings.Contains(out, "egist") {
		t.Fatal("unknown registration should add nothing")
	}
}
