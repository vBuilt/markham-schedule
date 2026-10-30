package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

var (
	ErrBot            = errors.New("bot check failed")
	ErrBotUnavailable = errors.New("bot check unavailable")
)

// BotCheck verifies a client-side challenge token.
type BotCheck interface {
	Verify(ctx context.Context, token, ip string) error
}

const defaultTurnstileEndpoint = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// Turnstile verifies Cloudflare Turnstile tokens.
type Turnstile struct {
	Secret, Endpoint string
	HTTP             *http.Client
}

func (t *Turnstile) Verify(ctx context.Context, token, ip string) error {
	if token == "" {
		return ErrBot
	}
	endpoint := t.Endpoint
	if endpoint == "" {
		endpoint = defaultTurnstileEndpoint
	}
	form := url.Values{"secret": {t.Secret}, "response": {token}, "remoteip": {ip}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return ErrBotUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient(t.HTTP).Do(req)
	if err != nil {
		return ErrBotUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ErrBotUnavailable
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ErrBotUnavailable
	}
	if !out.Success {
		return ErrBot
	}
	return nil
}

// NoBotCheck accepts everything (development, or no Turnstile configured).
type NoBotCheck struct{}

func (NoBotCheck) Verify(context.Context, string, string) error { return nil }
