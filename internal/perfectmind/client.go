package perfectmind

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultBaseURL  = "https://cityofmarkham.perfectmind.com"
	defaultWidgetID = "6825ea71-e5b7-4c2a-948f-9195507ad90a"
	defaultUA       = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"
	dateLayout      = "2006-01-02"
	filterDateFmt   = "2006-01-02T00:00:00.000Z"
	maxBodyBytes    = 32 << 20
)

// Options configures a Client. Zero values select the defaults noted.
type Options struct {
	BaseURL, WidgetID, UserAgent string
	Delay                        time.Duration // between every request, including token GETs (none before the first)
	Timeout                      time.Duration // per request, default 30s
	Retries                      int           // per POST on non-JSON / 5xx / network error; 0 = default 2, negative = no retries
	RetryBackoff                 time.Duration // base backoff, multiplied by the attempt number; default 3s
	MaxPages                     int           // default 40
	HTTPClient                   *http.Client  // optional; a cookie jar is added if it has none
	Logger                       *slog.Logger  // default slog.Default()
}

// Client talks to the PerfectMind widget API. It is not safe for concurrent use:
// requests are deliberately sequential.
type Client struct {
	o     Options
	hc    *http.Client
	log   *slog.Logger
	count int // requests made so far
}

func New(o Options) *Client {
	if o.BaseURL == "" {
		o.BaseURL = defaultBaseURL
	}
	o.BaseURL = strings.TrimRight(o.BaseURL, "/")
	if o.WidgetID == "" {
		o.WidgetID = defaultWidgetID
	}
	if o.UserAgent == "" {
		o.UserAgent = defaultUA
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.Retries == 0 {
		o.Retries = 2
	} else if o.Retries < 0 {
		o.Retries = 0
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = 3 * time.Second
	}
	if o.MaxPages <= 0 {
		o.MaxPages = 40
	}
	c := &Client{o: o, log: o.Logger}
	if c.log == nil {
		c.log = slog.Default()
	}
	jar, _ := cookiejar.New(nil)
	switch {
	case o.HTTPClient == nil:
		c.hc = &http.Client{Jar: jar, Timeout: o.Timeout}
	case o.HTTPClient.Jar == nil:
		cp := *o.HTTPClient
		cp.Jar = jar
		c.hc = &cp
	default:
		c.hc = o.HTTPClient
	}
	return c
}

// AbsURL joins BaseURL with a site-relative path; absolute URLs are returned unchanged.
func (c *Client) AbsURL(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.o.BaseURL + path
}

// Calendars returns every calendar that has a booking link, flattened across groups.
func (c *Client) Calendars(ctx context.Context) ([]Calendar, error) {
	page := fmt.Sprintf("%s/Clients/BookMe4?widgetId=%s", c.o.BaseURL, url.QueryEscape(c.o.WidgetID))
	post := c.o.BaseURL + "/Clients/BookMe4V2/GetCategoriesDataV2?embed=False"
	tok, err := c.token(ctx, page)
	if err != nil {
		return nil, fmt.Errorf("perfectmind: category page: %w", err)
	}
	today := time.Now().UTC()
	var groups []CategoryGroup
	form := func(tok string) url.Values {
		v := url.Values{"widgetId": {c.o.WidgetID}, "__RequestVerificationToken": {tok}}
		addFilters(v, today, today.AddDate(1, 0, 0))
		return v
	}
	if err := c.postJSON(ctx, page, post, &tok, form, &groups, nil); err != nil {
		return nil, fmt.Errorf("perfectmind: categories: %w", err)
	}
	var out []Calendar
	for _, g := range groups {
		for _, cal := range g.Calendars {
			if cal.BookingLink == "" {
				c.log.Info("skipping calendar without booking link", "calendar", cal.Name)
				continue
			}
			out = append(out, cal)
		}
	}
	return out, nil
}

// Classes returns the class occurrences of one calendar between from and to (dates in local terms).
func (c *Client) Classes(ctx context.Context, cal Calendar, from, to time.Time) ([]RawClass, error) {
	page := c.AbsURL(cal.BookingLink)
	if cal.BookingLink == "" {
		page = fmt.Sprintf("%s/Clients/BookMe4BookingPages/Classes?calendarId=%s&widgetId=%s&embed=False",
			c.o.BaseURL, url.QueryEscape(cal.ID), url.QueryEscape(c.o.WidgetID))
	}
	post := c.o.BaseURL + "/Clients/BookMe4BookingPagesV2/ClassesV2"
	tok, err := c.token(ctx, page)
	if err != nil {
		return nil, fmt.Errorf("perfectmind: calendar %q: %w", cal.Name, err)
	}
	start, _ := time.Parse(dateLayout, from.Format(dateLayout))
	end, _ := time.Parse(dateLayout, to.Format(dateLayout))
	var all []RawClass
	for pageNo := 0; ; pageNo++ {
		if pageNo >= c.o.MaxPages {
			c.log.Warn("page cap reached, returning partial results", "calendar", cal.Name, "pages", pageNo, "classes", len(all))
			break
		}
		form := func(tok string) url.Values {
			v := url.Values{"calendarId": {cal.ID}, "widgetId": {c.o.WidgetID}, "page": {"0"}, "__RequestVerificationToken": {tok}}
			addFilters(v, start, end)
			return v
		}
		var resp ClassesResponse
		// The real end-of-data page is {"classes":[],"nextKey":"0001-01-01"}; no
		// classes and no nextKey means a body we do not understand, not "empty".
		check := func() error {
			if len(resp.Classes) == 0 && resp.NextKey == "" {
				return fmt.Errorf("POST %s: unexpected response shape (no classes and no nextKey)", post)
			}
			return nil
		}
		if err := c.postJSON(ctx, page, post, &tok, form, &resp, check); err != nil {
			return nil, fmt.Errorf("perfectmind: calendar %q: %w", cal.Name, err)
		}
		if len(resp.Classes) == 0 {
			break
		}
		all = append(all, resp.Classes...)
		nk, err := time.Parse(dateLayout, resp.NextKey)
		if err != nil {
			c.log.Warn("unparseable nextKey, stopping", "calendar", cal.Name, "nextKey", resp.NextKey)
			break
		}
		next := nk.AddDate(0, 0, 1)
		if !next.After(start) || !next.Before(end) {
			break
		}
		start = next
	}
	return all, nil
}

// addFilters adds the widget's filter values; Date Range is the only one that matters.
func addFilters(v url.Values, from, to time.Time) {
	set := func(i int, name, val, val2, kind string) {
		p := fmt.Sprintf("values[%d]", i)
		v.Set(p+"[Name]", name)
		v.Set(p+"[Value]", val)
		v.Set(p+"[Value2]", val2)
		v.Set(p+"[ValueKind]", kind)
	}
	set(0, "Date Range", from.Format(filterDateFmt), to.Format(filterDateFmt), "6")
	set(1, "Time Range", "", "", "7")
	set(2, "Keyword", "", "", "9")
	set(3, "Age", "0", "1188", "0")
}

var (
	tokenTagRe   = regexp.MustCompile(`(?is)<input\b[^>]*\bname\s*=\s*["']__RequestVerificationToken["'][^>]*>`)
	tokenValueRe = regexp.MustCompile(`(?is)\bvalue\s*=\s*["']([^"']*)["']`)
)

// wait enforces Delay before every request except the first.
func (c *Client) wait(ctx context.Context) error {
	first := c.count == 0
	c.count++
	if first || c.o.Delay <= 0 {
		return ctx.Err()
	}
	return sleep(ctx, c.o.Delay)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// token GETs an HTML page (setting cookies) and extracts the anti-forgery token.
func (c *Client) token(ctx context.Context, pageURL string) (string, error) {
	if err := c.wait(ctx); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.o.UserAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d", pageURL, resp.StatusCode)
	}
	tag := tokenTagRe.Find(body)
	if m := tokenValueRe.FindSubmatch(tag); tag != nil && m != nil {
		return html.UnescapeString(string(m[1])), nil
	}
	return "", fmt.Errorf("GET %s: no verification token in page (final URL %s)", pageURL, resp.Request.URL)
}

// postJSON posts the form and decodes a JSON response into out, retrying
// non-JSON, 5xx, network failures and (when check is non-nil) decoded bodies
// that check rejects. Before retrying after a non-network failure it re-GETs
// the page to refresh token and cookies.
func (c *Client) postJSON(ctx context.Context, pageURL, postURL string, tok *string, form func(string) url.Values, out any, check func() error) error {
	var lastErr error
	for attempt := 0; attempt <= c.o.Retries; attempt++ {
		if attempt > 0 {
			c.log.Warn("retrying PerfectMind request", "url", postURL, "attempt", attempt, "err", lastErr)
			if err := sleep(ctx, c.o.RetryBackoff*time.Duration(attempt)); err != nil {
				return err
			}
			if _, isNet := lastErr.(netErr); !isNet {
				t, err := c.token(ctx, pageURL)
				if err != nil {
					lastErr = err
					if ctx.Err() != nil {
						return ctx.Err()
					}
					continue
				}
				*tok = t
			}
		}
		err := c.postOnce(ctx, pageURL, postURL, form(*tok), out)
		if err == nil && check != nil {
			err = check()
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
	}
	return lastErr
}

type netErr struct{ error }

func (c *Client) postOnce(ctx context.Context, pageURL, postURL string, form url.Values, out any) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, postURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.o.UserAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", pageURL)
	resp, err := c.hc.Do(req)
	if err != nil {
		return netErr{err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return netErr{err}
	}
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != http.StatusOK || !strings.Contains(strings.ToLower(ct), "application/json") {
		return fmt.Errorf("POST %s: non-JSON response (status %d, content-type %q, final URL %s)", postURL, resp.StatusCode, ct, resp.Request.URL.Host)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("POST %s: decode: %w", postURL, err)
	}
	return nil
}

// Registration fetches one occurrence's class landing page and returns when
// online registration opens for residents, members and the public. occurrenceDate
// is YYYYMMDD. It is a plain GET (no token) and honours Delay like every request.
func (c *Client) Registration(ctx context.Context, eventID, occurrenceDate string, loc *time.Location) (residents, members, public *time.Time, err error) {
	if err := c.wait(ctx); err != nil {
		return nil, nil, nil, err
	}
	q := url.Values{
		"widgetId":                  {c.o.WidgetID},
		"redirectedFromEmbededMode": {"False"},
		"classId":                   {eventID},
		"occurrenceDate":            {occurrenceDate},
	}
	pageURL := c.AbsURL("/Clients/BookMe4LandingPages/Class?" + q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("User-Agent", c.o.UserAgent)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, fmt.Errorf("GET class %s/%s: status %d", eventID, occurrenceDate, resp.StatusCode)
	}
	residents, members, public, err = ParseRegistration(body, loc)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("class %s/%s: %w", eventID, occurrenceDate, err)
	}
	return residents, members, public, nil
}
