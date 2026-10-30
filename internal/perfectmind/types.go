// Package perfectmind talks to the City of Markham PerfectMind widget.
package perfectmind

type Calendar struct {
	ID          string `json:"Id"`
	Name        string `json:"Name"`
	BookingLink string `json:"BookingLink"` // empty when the API returns null
}

type CategoryGroup struct {
	Name      string     `json:"Name"`
	Calendars []Calendar `json:"Calendars"`
}

type RawAddress struct {
	AddressTag string `json:"AddressTag"`
	Street     string `json:"Street"`
	City       string `json:"City"`
	PostalCode string `json:"PostalCode"`
}

// RawClass holds the useful fields of a class occurrence from ClassesV2.
type RawClass struct {
	EventId            string     `json:"EventId"`
	CourseId           string     `json:"CourseId"`
	EventName          string     `json:"EventName"`
	Details            string     `json:"Details"`
	OccurrenceDate     string     `json:"OccurrenceDate"` // YYYYMMDD
	FormattedStartTime string     `json:"FormattedStartTime"`
	FormattedEndTime   string     `json:"FormattedEndTime"`
	DurationInMinutes  int        `json:"DurationInMinutes"`
	AllDayEvent        bool       `json:"AllDayEvent"`
	Facility           string     `json:"Facility"`
	Address            RawAddress `json:"Address"`
	Location           string     `json:"Location"`
	AgeRestrictions    string     `json:"AgeRestrictions"`
	MinAge             *int       `json:"MinAge"`
	MaxAge             *int       `json:"MaxAge"`
	PriceRange         string     `json:"PriceRange"`
	Spots              string     `json:"Spots"`
	BookButtonText     string     `json:"BookButtonText"`
}

type ClassesResponse struct {
	Classes []RawClass `json:"classes"`
	NextKey string     `json:"nextKey"`
}
