// Package web holds the browser calendar UI, embedded into the binary.
package web

import (
	"bytes"
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
)

//go:embed index.html
var index []byte

//go:embed status.html
var status []byte

//go:embed login.html
var login []byte

//go:embed forgot.html
var forgot []byte

//go:embed reset.html
var reset []byte

//go:embed verify.html
var verify []byte

//go:embed account.html
var account []byte

// Index serves the single-page calendar UI.
func Index(w http.ResponseWriter, r *http.Request) { write(w, index) }

// Status serves the scrape status page.
func Status(w http.ResponseWriter, r *http.Request) { write(w, status) }

// write serves an HTML page that may not be framed by other sites
// (clickjacking protection for the sign-in and account forms).
func write(w http.ResponseWriter, b []byte) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	_, _ = w.Write(b)
}

func page(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { write(w, b) }
}

// Account pages.
var (
	Login   = page(login)
	Forgot  = page(forgot)
	Reset   = page(reset)
	Verify  = page(verify)
	Account = page(account)
)

//go:embed watchstop.html
var watchStop string

var watchStopTmpl = template.Must(template.New("watchstop").Parse(watchStop))

// stopPage renders the shared single-action confirm page. path is the form
// action; apiToken, if any, is carried on it and on the calendar link.
func stopPage(w http.ResponseWriter, title, heading, button, path, msg, token, apiToken string) {
	home, action := "/", path
	if apiToken != "" {
		q := url.QueryEscape(apiToken)
		home, action = "/?token="+q, path+"?token="+q
	}
	var buf bytes.Buffer
	if err := watchStopTmpl.Execute(&buf, struct {
		Title, Heading, Button, Message, Token string
		Home, Action                           template.URL
	}{title, heading, button, msg, token, template.URL(home), template.URL(action)}); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	write(w, buf.Bytes())
}

// WatchStop serves the stop-link page; msg is chosen by the server. A
// non-empty stopToken adds a button that POSTs it back to /watch/stop.
// apiToken, if any, is carried on the form action and the calendar link.
func WatchStop(w http.ResponseWriter, msg, stopToken, apiToken string) {
	stopPage(w, "Watch stopped", "Spot watch", "Stop watching", "/watch/stop", msg, stopToken, apiToken)
}

// ReminderCancel serves the reminder cancel-link page, like WatchStop but
// POSTing to /reminder/cancel.
func ReminderCancel(w http.ResponseWriter, msg, cancelToken, apiToken string) {
	stopPage(w, "Reminder cancelled", "Registration reminder", "Cancel reminder", "/reminder/cancel", msg, cancelToken, apiToken)
}
