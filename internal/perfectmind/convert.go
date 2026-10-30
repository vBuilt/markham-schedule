package perfectmind

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vBuilt/markham-schedule/internal/schedule"
)

const timeLayout = "20060102 03:04 PM"

// SessionID is the first 16 hex chars of sha1(EventId|OccurrenceDate|start|Facility).
func SessionID(r RawClass) string {
	sum := sha1.Sum([]byte(r.EventId + "|" + r.OccurrenceDate + "|" + r.FormattedStartTime + "|" + r.Facility))
	return hex.EncodeToString(sum[:])[:16]
}

var spotsRe = regexp.MustCompile(`^\s*(\d+)\s+spots?\b`)

// ParseSpots returns the number of spots left, or nil when not numeric.
func ParseSpots(s string) *int {
	m := spotsRe.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return nil
	}
	return &n
}

func ToSession(r RawClass, category, bookingURL string, loc *time.Location, scrapedAt time.Time) (schedule.Session, error) {
	var start, end time.Time
	if r.AllDayEvent {
		day, err := time.ParseInLocation("20060102", r.OccurrenceDate, loc)
		if err != nil {
			return schedule.Session{}, fmt.Errorf("parse occurrence date %q: %w", r.OccurrenceDate, err)
		}
		start = day
		end = time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc)
	} else {
		var err error
		start, err = time.ParseInLocation(timeLayout, r.OccurrenceDate+" "+r.FormattedStartTime, loc)
		if err != nil {
			return schedule.Session{}, fmt.Errorf("parse start %q %q: %w", r.OccurrenceDate, r.FormattedStartTime, err)
		}
		if r.DurationInMinutes > 0 {
			end = start.Add(time.Duration(r.DurationInMinutes) * time.Minute)
		} else {
			end, err = time.ParseInLocation(timeLayout, r.OccurrenceDate+" "+r.FormattedEndTime, loc)
			if err != nil {
				return schedule.Session{}, fmt.Errorf("parse end %q %q: %w", r.OccurrenceDate, r.FormattedEndTime, err)
			}
			if end.Before(start) {
				end = time.Date(end.Year(), end.Month(), end.Day()+1, end.Hour(), end.Minute(), 0, 0, loc)
			}
		}
	}
	return schedule.Session{
		ID:          SessionID(r),
		Categories:  []string{category},
		Name:        strings.Join(strings.Fields(r.EventName), " "),
		Description: strings.TrimSpace(strings.ReplaceAll(r.Details, "\r\n", "\n")),
		Start:       start,
		End:         end,
		AllDay:      r.AllDayEvent,
		Centre:      r.Address.AddressTag,
		Address:     schedule.Address{Street: r.Address.Street, City: r.Address.City, PostalCode: r.Address.PostalCode},
		Facility:    r.Facility,
		Age:         r.AgeRestrictions,
		MinAge:      r.MinAge,
		MaxAge:      r.MaxAge,
		Price:       r.PriceRange,
		Spots:       r.Spots,
		SpotsLeft:   ParseSpots(r.Spots),
		CourseID:    r.CourseId,
		BookingURL:  bookingURL,
		ScrapedAt:   scrapedAt,
	}, nil
}
