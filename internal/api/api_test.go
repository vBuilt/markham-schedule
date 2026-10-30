package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vBuilt/markham-schedule/internal/schedule"
	"github.com/vBuilt/markham-schedule/internal/scraper"
	"github.com/vBuilt/markham-schedule/internal/store"
)

type fakeStore struct {
	sessions []schedule.Session
	cats     []store.CategoryCount
	centres  []store.CentreCount
	acts     []store.ActivityCount
	runs     []schedule.Run
	gotLimit int
	status   schedule.Status
	err      error
	got      schedule.Filter
}

func (f *fakeStore) Sessions(_ context.Context, fl schedule.Filter) ([]schedule.Session, error) {
	f.got = fl
	return f.sessions, f.err
}
func (f *fakeStore) Categories(context.Context) ([]store.CategoryCount, error) { return f.cats, f.err }
func (f *fakeStore) Centres(context.Context) ([]store.CentreCount, error)      { return f.centres, f.err }
func (f *fakeStore) Activities(context.Context) ([]store.ActivityCount, error) { return f.acts, f.err }
func (f *fakeStore) Runs(_ context.Context, limit int) ([]schedule.Run, error) {
	f.gotLimit = limit
	return f.runs, f.err
}
func (f *fakeStore) LoadStatus(context.Context) (schedule.Status, error) { return f.status, f.err }

var toronto, _ = time.LoadLocation("America/Toronto")
var fixedNow = time.Date(2026, 10, 30, 15, 30, 0, 0, toronto)

func newH(fs *fakeStore, o Options) http.Handler {
	o.Now = func() time.Time { return fixedNow }
	if o.Retention == 0 {
		o.Retention = 336 * time.Hour
	}
	return New(fs, o)
}

func get(h http.Handler, target string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSessionsFilters(t *testing.T) {
	fs := &fakeStore{sessions: []schedule.Session{{ID: "a", Name: "Aqua"}}}
	w := get(newH(fs, Options{}), "/api/sessions?category=AQUAFIT&centre=cornell&centre=angus+glen&q=lane&category=")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := fs.got.Categories; len(got) != 1 || got[0] != "AQUAFIT" {
		t.Fatalf("cats %v", got)
	}
	if got := fs.got.Centres; len(got) != 2 || got[0] != "cornell" || got[1] != "angus glen" {
		t.Fatalf("centres %v", got)
	}
	if fs.got.Query != "lane" || fs.got.Limit != 500 {
		t.Fatalf("%+v", fs.got)
	}
	var resp struct {
		Count    int                `json:"count"`
		Sessions []schedule.Session `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Count != 1 || len(resp.Sessions) != 1 {
		t.Fatal(err, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatal(ct)
	}
}

func TestSessionsBadParams(t *testing.T) {
	h := newH(&fakeStore{}, Options{})
	for _, u := range []string{"/api/sessions?from=2026-13-01", "/api/sessions?to=nope", "/api/sessions?limit=abc"} {
		w := get(h, u)
		if w.Code != 400 {
			t.Fatal(u, w.Code)
		}
		var e map[string]string
		if json.Unmarshal(w.Body.Bytes(), &e) != nil || e["error"] == "" {
			t.Fatal(u, w.Body.String())
		}
	}
}

func TestSessionsLimitClampAndRange(t *testing.T) {
	fs := &fakeStore{}
	get(newH(fs, Options{}), "/api/sessions?limit=999999&from=2026-11-01&to=2026-11-03")
	if fs.got.Limit != 5000 {
		t.Fatal(fs.got.Limit)
	}
	if !fs.got.From.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, toronto)) ||
		!fs.got.To.Equal(time.Date(2026, 11, 4, 0, 0, 0, 0, toronto)) {
		t.Fatalf("%v %v", fs.got.From, fs.got.To)
	}
}

func TestSessionsDefaultFromToday(t *testing.T) {
	fs := &fakeStore{}
	get(newH(fs, Options{}), "/api/sessions")
	if !fs.got.From.Equal(time.Date(2026, 10, 30, 0, 0, 0, 0, toronto)) || !fs.got.To.IsZero() {
		t.Fatalf("%v %v", fs.got.From, fs.got.To)
	}
}

func TestCalendarWindowAndType(t *testing.T) {
	fs := &fakeStore{}
	h := newH(fs, Options{CalName: "Markham Drop-In", Refresh: 3 * time.Hour})
	w := get(h, "/calendar.ics")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/calendar; charset=utf-8" {
		t.Fatal(w.Code, w.Header())
	}
	if !fs.got.From.Equal(fixedNow.Add(-336*time.Hour)) || !fs.got.To.Equal(fixedNow.Add(60*24*time.Hour)) || fs.got.Limit != 0 {
		t.Fatalf("%+v", fs.got)
	}
	get(h, "/calendar.ics?days=7")
	if !fs.got.To.Equal(fixedNow.Add(7 * 24 * time.Hour)) {
		t.Fatal(fs.got.To)
	}
	for _, d := range []string{"0", "366", "x"} {
		if w := get(h, "/calendar.ics?days="+d); w.Code != 400 {
			t.Fatal(d, w.Code)
		}
	}
}

func TestEmptyStore(t *testing.T) {
	h := newH(&fakeStore{}, Options{CalName: "X"})
	w := get(h, "/api/sessions")
	if strings.TrimSpace(w.Body.String()) != `{"updated_at":null,"count":0,"sessions":[]}` {
		t.Fatal(w.Body.String())
	}
	w = get(h, "/calendar.ics")
	b := w.Body.String()
	if w.Code != 200 || !strings.Contains(b, "BEGIN:VCALENDAR") || !strings.Contains(b, "END:VCALENDAR") {
		t.Fatal(w.Code, b)
	}
}

func TestTokenAuth(t *testing.T) {
	h := newH(&fakeStore{}, Options{Token: "sekret"})
	if w := get(h, "/api/sessions"); w.Code != 401 || !strings.Contains(w.Body.String(), `"unauthorized"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := get(h, "/api/sessions?token=bad"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := get(h, "/api/sessions?token=sekret"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := get(h, "/calendar.ics", "Authorization", "Bearer sekret"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := get(h, "/healthz"); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "ok" {
		t.Fatal(w.Code)
	}
}

func TestCategoriesCentresStatus(t *testing.T) {
	h := newH(&fakeStore{}, Options{Version: "v1"})
	for _, u := range []string{"/api/categories", "/api/centres"} {
		if w := get(h, u); strings.TrimSpace(w.Body.String()) != "[]" {
			t.Fatal(u, w.Body.String())
		}
	}
	w := get(h, "/api/status")
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["version"] != "v1" || m["updated_at"] != nil {
		t.Fatal(m)
	}
	if c, ok := m["categories"].([]any); !ok || len(c) != 0 {
		t.Fatal(m["categories"])
	}

	ts := fixedNow
	fs := &fakeStore{
		cats:    []store.CategoryCount{{Name: "AQUAFIT", Count: 3}},
		centres: []store.CentreCount{{Name: "Cornell", Count: 2}},
		status:  schedule.Status{LastSuccess: &ts, Total: 3},
	}
	h = newH(fs, Options{Version: "v1"})
	if w := get(h, "/api/categories"); !strings.Contains(w.Body.String(), `"name":"AQUAFIT"`) {
		t.Fatal(w.Body.String())
	}
	if w := get(h, "/api/centres"); !strings.Contains(w.Body.String(), `"address"`) {
		t.Fatal(w.Body.String())
	}
	json.Unmarshal(get(h, "/api/status").Body.Bytes(), &m)
	if m["updated_at"] == nil || m["total"].(float64) != 3 {
		t.Fatal(m)
	}
}

func TestStoreError(t *testing.T) {
	h := newH(&fakeStore{err: errors.New("boom")}, Options{})
	for _, u := range []string{"/api/sessions", "/api/categories", "/api/centres", "/api/status", "/calendar.ics"} {
		w := get(h, u)
		if w.Code != 500 || strings.Contains(w.Body.String(), "boom") || !strings.Contains(w.Body.String(), "internal error") {
			t.Fatal(u, w.Code, w.Body.String())
		}
	}
}

func TestIndexPage(t *testing.T) {
	h := newH(&fakeStore{}, Options{})
	w := get(h, "/")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatal(w.Code, w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Body.String(), "<title>Markham drop-in</title>") {
		t.Fatal("index page not served")
	}
	if w := get(h, "/nope"); w.Code != 404 {
		t.Fatal("unknown path should 404, got", w.Code)
	}

	ht := newH(&fakeStore{}, Options{Token: "sekret"})
	if w := get(ht, "/"); w.Code != 401 {
		t.Fatal("index without token should 401, got", w.Code)
	}
	if w := get(ht, "/?token=sekret"); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestActivityFilterAndList(t *testing.T) {
	fs := &fakeStore{acts: []store.ActivityCount{{Category: "Swimming", Name: "Drop-In Lane Swim", Count: 3}}}
	h := newH(fs, Options{})
	if w := get(h, "/api/sessions?activity=Drop-In+Lane+Swim&activity=&category=Aquafit"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if len(fs.got.Activities) != 1 || fs.got.Activities[0] != "Drop-In Lane Swim" || len(fs.got.Categories) != 1 {
		t.Fatalf("filter %+v", fs.got)
	}
	if w := get(h, "/calendar.ics?activity=Drop-In+Lane+Swim"); w.Code != 200 || len(fs.got.Activities) != 1 {
		t.Fatal("calendar should accept activity", w.Code)
	}
	w := get(h, "/api/activities")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `{"category":"Swimming","name":"Drop-In Lane Swim","count":3}`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := get(newH(&fakeStore{}, Options{}), "/api/activities"); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal("empty list should be [], got", w.Body.String())
	}
}

func TestRunsAndStatusPage(t *testing.T) {
	fs := &fakeStore{runs: []schedule.Run{{OK: true, Total: 7, DurationMS: 1500}}}
	h := newH(fs, Options{Refresh: 3 * time.Hour})
	w := get(h, "/api/runs?limit=5")
	if w.Code != 200 || fs.gotLimit != 5 || !strings.Contains(w.Body.String(), `"total":7`) {
		t.Fatal(w.Code, fs.gotLimit, w.Body.String())
	}
	if w := get(h, "/api/runs?limit=9999"); w.Code != 200 || fs.gotLimit != 300 {
		t.Fatal("limit should clamp to 300, got", fs.gotLimit)
	}
	if w := get(h, "/api/runs?limit=x"); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := get(newH(&fakeStore{}, Options{}), "/api/runs"); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal("empty runs should be [], got", w.Body.String())
	}
	if w := get(h, "/status"); w.Code != 200 || !strings.Contains(w.Body.String(), "<title>Scrape status") {
		t.Fatal("status page", w.Code)
	}
	if w := get(newH(&fakeStore{}, Options{Token: "k"}), "/status"); w.Code != 401 {
		t.Fatal("status page should need the token", w.Code)
	}
}

func TestStatusIncludesIntervalAndRegistrationCoverage(t *testing.T) {
	at := fixedNow
	fs := &fakeStore{sessions: []schedule.Session{
		{Registration: &schedule.Registration{Residents: &at}},
		{Registration: &schedule.Registration{}},
		{},
	}}
	h := newH(fs, Options{Refresh: 3 * time.Hour})
	body := get(h, "/api/status").Body.String()
	for _, want := range []string{`"refresh_interval_seconds":10800`, `"registration":{"online":1,"in_person":1,"unknown":1}`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
}

type fakeRefresher struct {
	err        error
	refreshing bool
	at         time.Time
	calls      int
}

func (f *fakeRefresher) RequestRefresh(context.Context) error { f.calls++; return f.err }
func (f *fakeRefresher) Refreshing() bool                     { return f.refreshing }
func (f *fakeRefresher) ManualRefreshAt(context.Context) (time.Time, error) {
	return f.at, nil
}

func post(h http.Handler, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", target, nil))
	return w
}

func TestRefreshNow(t *testing.T) {
	fr := &fakeRefresher{}
	h := newH(&fakeStore{}, Options{Refresher: fr})
	if w := post(h, "/api/refresh"); w.Code != 202 || fr.calls != 1 {
		t.Fatal(w.Code, fr.calls, w.Body.String())
	}
	fr.err = scraper.ErrBusy
	if w := post(h, "/api/refresh"); w.Code != 409 {
		t.Fatal("busy:", w.Code)
	}
	fr.err = &scraper.TooSoonError{Wait: 90 * time.Second}
	w := post(h, "/api/refresh")
	if w.Code != 429 || w.Header().Get("Retry-After") != "90" || !strings.Contains(w.Body.String(), `"retry_after_seconds":90`) {
		t.Fatal("too soon:", w.Code, w.Header(), w.Body.String())
	}
	if w := get(h, "/api/refresh"); w.Code != 405 {
		t.Fatal("GET should be 405, got", w.Code)
	}
	if w := post(newH(&fakeStore{}, Options{}), "/api/refresh"); w.Code != 404 {
		t.Fatal("no refresher should be 404, got", w.Code)
	}
	if w := post(newH(&fakeStore{}, Options{Token: "k", Refresher: fr}), "/api/refresh"); w.Code != 401 {
		t.Fatal("should need the token", w.Code)
	}
}

func TestStatusShowsManualRefresh(t *testing.T) {
	at := fixedNow.Add(5 * time.Minute)
	h := newH(&fakeStore{}, Options{Refresher: &fakeRefresher{refreshing: true, at: at}})
	body := get(h, "/api/status").Body.String()
	for _, want := range []string{`"refreshing":true`, `"refresh_now":true`, `"refresh_now_at":"` + at.Format(time.RFC3339)} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	body = get(newH(&fakeStore{}, Options{}), "/api/status").Body.String()
	if !strings.Contains(body, `"refresh_now":false`) || !strings.Contains(body, `"refresh_now_at":null`) {
		t.Fatal(body)
	}
}
