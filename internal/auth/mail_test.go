package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResendMailer(t *testing.T) {
	var body map[string]any
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("bad request: %s %q", r.Method, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	m := &ResendMailer{APIKey: "k", From: "F <f@x.com>", Endpoint: srv.URL}
	if err := m.Send(context.Background(), "a@x.com", "subj", "txt", "<p>h</p>"); err != nil {
		t.Fatal(err)
	}
	to, _ := body["to"].([]any)
	if body["from"] != "F <f@x.com>" || len(to) != 1 || to[0] != "a@x.com" ||
		body["subject"] != "subj" || body["text"] != "txt" || body["html"] != "<p>h</p>" {
		t.Errorf("body = %v", body)
	}
	status = http.StatusUnprocessableEntity
	err := m.Send(context.Background(), "a@x.com", "s", "t", "h")
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Errorf("err = %v", err)
	}
}
