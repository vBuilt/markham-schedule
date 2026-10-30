// Package web holds the browser calendar UI, embedded into the binary.
package web

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var index []byte

// Index serves the single-page calendar UI.
func Index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(index)
}
