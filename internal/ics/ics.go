// Package ics renders sessions as an RFC 5545 iCalendar feed.
package ics

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	_ "time/tzdata"

	"github.com/vBuilt/markham-schedule/internal/schedule"
)

type Options struct {
	Name    string
	Refresh time.Duration
	Now     time.Time
}

const vtimezone = `BEGIN:VTIMEZONE
TZID:America/Toronto
BEGIN:DAYLIGHT
TZOFFSETFROM:-0500
TZOFFSETTO:-0400
TZNAME:EDT
DTSTART:19700308T020000
RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=2SU
END:DAYLIGHT
BEGIN:STANDARD
TZOFFSETFROM:-0400
TZOFFSETTO:-0500
TZNAME:EST
DTSTART:19701101T020000
RRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU
END:STANDARD
END:VTIMEZONE`

var textEscaper = strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`)

func esc(s string) string { return textEscaper.Replace(s) }

// writeLine writes one content line folded at 75 octets, never splitting a rune.
func writeLine(w *bufio.Writer, line string) {
	limit := 75
	for len(line) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		if cut == 0 { // cannot happen with valid input; avoid looping forever
			cut = limit
		}
		w.WriteString(line[:cut] + "\r\n")
		line = " " + line[cut:]
		limit = 75
	}
	w.WriteString(line + "\r\n")
}

func location(s schedule.Session) string {
	head := s.Centre
	if s.Centre != "" && s.Facility != "" {
		head = s.Centre + " – " + s.Facility
	} else if s.Facility != "" {
		head = s.Facility
	}
	var parts []string
	for _, p := range []string{head, s.Address.Street, s.Address.City} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

func description(s schedule.Session, toronto *time.Location) string {
	var lines []string
	add := func(k, v string) {
		if v != "" {
			lines = append(lines, k+": "+v)
		}
	}
	add("Category", strings.Join(s.Categories, ", "))
	add("Age", s.Age)
	add("Price", s.Price)
	if s.Spots != "" {
		lines = append(lines, fmt.Sprintf("Spots: %s (as of %s)", s.Spots, s.ScrapedAt.In(toronto).Format("Jan 2, 3:04 PM")))
	}
	add("Course ID", s.CourseID)
	out := strings.Join(lines, "\n")
	if s.Description != "" {
		out += "\n\n" + s.Description
	}
	if s.BookingURL != "" {
		out += "\n\n" + s.BookingURL
	}
	return out
}

// Write renders the calendar. Events are emitted in input order; the first
// occurrence of each session ID wins.
func Write(w io.Writer, sessions []schedule.Session, o Options) error {
	toronto, err := time.LoadLocation("America/Toronto")
	if err != nil {
		return err
	}
	hours := int(o.Refresh.Hours())
	if hours < 1 {
		hours = 1
	}
	bw := bufio.NewWriter(w)
	line := func(s string) { writeLine(bw, s) }

	line("BEGIN:VCALENDAR")
	line("VERSION:2.0")
	line("PRODID:-//markham-schedule//EN")
	line("CALSCALE:GREGORIAN")
	line("METHOD:PUBLISH")
	line("X-WR-CALNAME:" + esc(o.Name))
	line("X-WR-TIMEZONE:America/Toronto")
	line(fmt.Sprintf("REFRESH-INTERVAL;VALUE=DURATION:PT%dH", hours))
	line(fmt.Sprintf("X-PUBLISHED-TTL:PT%dH", hours))
	for _, l := range strings.Split(vtimezone, "\n") {
		line(l)
	}

	const stamp = "20060102T150405"
	seen := map[string]bool{}
	for _, s := range sessions {
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		line("BEGIN:VEVENT")
		line("UID:" + s.ID + "@markham-schedule")
		line("DTSTAMP:" + o.Now.UTC().Format(stamp) + "Z")
		if s.AllDay {
			st := s.Start.In(toronto)
			line("DTSTART;VALUE=DATE:" + st.Format("20060102"))
			line("DTEND;VALUE=DATE:" + st.AddDate(0, 0, 1).Format("20060102"))
		} else {
			line("DTSTART;TZID=America/Toronto:" + s.Start.In(toronto).Format(stamp))
			line("DTEND;TZID=America/Toronto:" + s.End.In(toronto).Format(stamp))
		}
		line("SUMMARY:" + esc(s.Name))
		if loc := location(s); loc != "" {
			line("LOCATION:" + esc(loc))
		}
		if d := description(s, toronto); d != "" {
			line("DESCRIPTION:" + esc(d))
		}
		if s.BookingURL != "" {
			line("URL:" + s.BookingURL)
		}
		line("END:VEVENT")
	}
	line("END:VCALENDAR")
	return bw.Flush()
}
