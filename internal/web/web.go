// Package web holds the browser calendar UI, embedded into the binary.
package web

import (
	_ "embed"
	"net/http"
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
