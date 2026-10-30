package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Mailer sends a transactional email.
type Mailer interface {
	Send(ctx context.Context, to, subject, text, html string) error
}

const defaultResendEndpoint = "https://api.resend.com/emails"

// ResendMailer sends mail through the Resend HTTP API.
type ResendMailer struct {
	APIKey, From, Endpoint string
	HTTP                   *http.Client
}

func (m *ResendMailer) Send(ctx context.Context, to, subject, text, html string) error {
	endpoint := m.Endpoint
	if endpoint == "" {
		endpoint = defaultResendEndpoint
	}
	body, err := json.Marshal(map[string]any{
		"from": m.From, "to": []string{to}, "subject": subject, "text": text, "html": html,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient(m.HTTP).Do(req)
	if err != nil {
		return fmt.Errorf("resend: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("resend: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// LogMailer logs mail instead of sending it (development).
type LogMailer struct{ Log *slog.Logger }

func (m LogMailer) Send(_ context.Context, to, subject, text, _ string) error {
	log := m.Log
	if log == nil {
		log = slog.Default()
	}
	log.Info("mail", "to", to, "subject", subject, "text", text)
	return nil
}

func httpClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 10 * time.Second}
}
