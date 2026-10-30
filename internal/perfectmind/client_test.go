package perfectmind

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "TOKEN&123="

type fakePM struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	starts     []string // posted values[0][Value] for ClassesV2
	htmlLeft   int      // number of upcoming ClassesV2 POSTs to answer with HTML
	htmlAlways bool
	pages      map[string]ClassesResponse // keyed by posted start date (YYYY-MM-DD)
	gets       int
	posts      int
}

func newFake(t *testing.T) *fakePM {
	f := &fakePM{t: t, pages: map[string]ClassesResponse{}}
	mux := http.NewServeMux()
	page := func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.gets++
		f.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "sess", Value: "abc", Path: "/"})
		w.Header().Set("Content-Type", "text/html")
		// attribute order differs between the two pages on purpose
		if strings.Contains(r.URL.Path, "BookingPages") {
			w.Write([]byte(`<html><input type="hidden" value="TOKEN&amp;123=" name="__RequestVerificationToken" /></html>`))
		} else {
			w.Write([]byte(`<html><input name="__RequestVerificationToken" type="hidden" value="TOKEN&amp;123=" /></html>`))
		}
	}
	mux.HandleFunc("/Clients/BookMe4", page)
	mux.HandleFunc("/Clients/BookMe4BookingPages/Classes", page)
	check := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		f.posts++
		f.mu.Unlock()
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", 400)
			return false
		}
		if r.PostForm.Get("__RequestVerificationToken") != testToken {
			http.Error(w, "no token", 400)
			return false
		}
		if _, err := r.Cookie("sess"); err != nil {
			http.Error(w, "no cookie", 400)
			return false
		}
		if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
			http.Error(w, "no xhr header", 400)
			return false
		}
		if r.Header.Get("Referer") == "" {
			http.Error(w, "no referer", 400)
			return false
		}
		return true
	}
	mux.HandleFunc("/Clients/BookMe4V2/GetCategoriesDataV2", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		b, _ := os.ReadFile("testdata/categories.json")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(b)
	})
	mux.HandleFunc("/Clients/BookMe4BookingPagesV2/ClassesV2", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		start := strings.TrimSuffix(r.PostForm.Get("values[0][Value]"), "T00:00:00.000Z")
		f.mu.Lock()
		f.starts = append(f.starts, start)
		html := f.htmlAlways || f.htmlLeft > 0
		if f.htmlLeft > 0 {
			f.htmlLeft--
		}
		resp, ok := f.pages[start]
		f.mu.Unlock()
		if html {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>Oops... there is an error</html>"))
			return
		}
		if !ok {
			b, _ := os.ReadFile("testdata/classes_empty.json")
			w.Header().Set("Content-Type", "application/json")
			w.Write(b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePM) client(mod func(*Options)) *Client {
	o := Options{BaseURL: f.srv.URL, WidgetID: "W", RetryBackoff: time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if mod != nil {
		mod(&o)
	}
	return New(o)
}

func fixtureClasses(t *testing.T, n int) []RawClass {
	b, err := os.ReadFile("testdata/classes_page1.json")
	if err != nil {
		t.Fatal(err)
	}
	var r ClassesResponse
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r.Classes[:n]
}

func day(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d
}

var testCal = Calendar{ID: "cal-1", Name: "Aquafit", BookingLink: "/Clients/BookMe4BookingPages/Classes?calendarId=cal-1&widgetId=W&embed=False"}

func TestCalendarsDropsNullBookingLink(t *testing.T) {
	f := newFake(t)
	cals, err := f.client(nil).Calendars(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cals {
		if c.BookingLink == "" {
			t.Errorf("calendar %q has empty BookingLink", c.Name)
		}
		if c.Name == "Aquafit" {
			found = true
		}
	}
	if !found {
		t.Error("Aquafit missing")
	}
	if len(cals) == 0 || len(cals) >= 14 {
		t.Errorf("expected fewer than 14 but >0 calendars, got %d", len(cals))
	}
}

func TestClassesPaginates(t *testing.T) {
	f := newFake(t)
	f.pages["2026-11-01"] = ClassesResponse{Classes: fixtureClasses(t, 3), NextKey: "2026-11-04"}
	f.pages["2026-11-05"] = ClassesResponse{Classes: fixtureClasses(t, 2), NextKey: "2026-11-09"}
	f.pages["2026-11-10"] = ClassesResponse{Classes: fixtureClasses(t, 4), NextKey: "2026-11-14"}
	// 2026-11-15 is not registered, so the fake answers with classes_empty.json
	got, err := f.client(nil).Classes(context.Background(), testCal, day("2026-11-01"), day("2026-12-31"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 9 {
		t.Errorf("got %d classes, want 9", len(got))
	}
	want := []string{"2026-11-01", "2026-11-05", "2026-11-10", "2026-11-15"}
	if strings.Join(f.starts, ",") != strings.Join(want, ",") {
		t.Errorf("starts = %v, want %v", f.starts, want)
	}
	if f.gets != 1 {
		t.Errorf("expected a single token GET per Classes call, got %d", f.gets)
	}
}

func TestClassesStopsAtHorizon(t *testing.T) {
	f := newFake(t)
	f.pages["2026-11-01"] = ClassesResponse{Classes: fixtureClasses(t, 3), NextKey: "2026-11-09"}
	got, err := f.client(nil).Classes(context.Background(), testCal, day("2026-11-01"), day("2026-11-10"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || len(f.starts) != 1 {
		t.Errorf("got %d classes in %d requests, want 3 in 1", len(got), len(f.starts))
	}
}

func TestClassesStopsWhenNextKeyStalls(t *testing.T) {
	f := newFake(t)
	f.pages["2026-11-01"] = ClassesResponse{Classes: fixtureClasses(t, 3), NextKey: "2026-11-05"}
	f.pages["2026-11-06"] = ClassesResponse{Classes: fixtureClasses(t, 3), NextKey: "2026-11-05"}
	got, err := f.client(nil).Classes(context.Background(), testCal, day("2026-11-01"), day("2026-12-31"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 || len(f.starts) != 2 {
		t.Errorf("got %d classes in %d requests, want 6 in 2", len(got), len(f.starts))
	}
}

func TestClassesMaxPagesIsNotAnError(t *testing.T) {
	f := newFake(t)
	f.pages["2026-11-01"] = ClassesResponse{Classes: fixtureClasses(t, 1), NextKey: "2026-11-04"}
	f.pages["2026-11-05"] = ClassesResponse{Classes: fixtureClasses(t, 1), NextKey: "2026-11-08"}
	got, err := f.client(func(o *Options) { o.MaxPages = 2 }).Classes(context.Background(), testCal, day("2026-11-01"), day("2026-12-31"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(f.starts) != 2 {
		t.Errorf("got %d classes in %d requests, want 2 in 2", len(got), len(f.starts))
	}
}

func TestClassesRetriesOnHTML(t *testing.T) {
	f := newFake(t)
	f.pages["2026-11-01"] = ClassesResponse{Classes: fixtureClasses(t, 3), NextKey: "2026-12-30"}
	f.htmlLeft = 1
	got, err := f.client(nil).Classes(context.Background(), testCal, day("2026-11-01"), day("2026-12-31"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("got %d classes, want 3", len(got))
	}
	if f.gets != 2 {
		t.Errorf("token page should be re-fetched after an HTML response; GETs = %d", f.gets)
	}

	f2 := newFake(t)
	f2.htmlAlways = true
	_, err = f2.client(nil).Classes(context.Background(), testCal, day("2026-11-01"), day("2026-12-31"))
	if err == nil || !strings.Contains(err.Error(), "Aquafit") {
		t.Fatalf("want error naming the calendar, got %v", err)
	}
	if len(f2.starts) != 3 { // 1 try + 2 retries
		t.Errorf("POST attempts = %d, want 3", len(f2.starts))
	}
}

func TestClassesSendsTokenAndCookie(t *testing.T) {
	f := newFake(t)
	f.pages["2026-11-01"] = ClassesResponse{Classes: fixtureClasses(t, 2), NextKey: "2026-12-30"}
	// The fake answers 400 (non-JSON) unless token, cookie, XHR header and referer are all present.
	got, err := f.client(func(o *Options) { o.Retries = -1 }).Classes(context.Background(), testCal, day("2026-11-01"), day("2026-12-31"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d classes, want 2", len(got))
	}
}

func TestDelayBetweenRequests(t *testing.T) {
	f := newFake(t)
	f.pages["2026-11-01"] = ClassesResponse{Classes: fixtureClasses(t, 1), NextKey: "2026-11-04"}
	f.pages["2026-11-05"] = ClassesResponse{Classes: fixtureClasses(t, 1), NextKey: "2026-12-30"}
	start := time.Now()
	// GET + POST + POST = 3 requests, so two delays.
	_, err := f.client(func(o *Options) { o.Delay = 50 * time.Millisecond }).Classes(context.Background(), testCal, day("2026-11-01"), day("2026-12-31"))
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Errorf("elapsed %v < 100ms", el)
	}
}

func TestDelayHonoursContext(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := f.client(func(o *Options) { o.Delay = time.Minute }).Calendars(ctx)
	if err == nil {
		t.Fatal("expected context error")
	}
}

func TestAbsURL(t *testing.T) {
	c := New(Options{BaseURL: "https://x.example/"})
	if got := c.AbsURL("/a/b?c=1"); got != "https://x.example/a/b?c=1" {
		t.Errorf("got %q", got)
	}
	if got := c.AbsURL("https://y.example/z"); got != "https://y.example/z" {
		t.Errorf("got %q", got)
	}
}
