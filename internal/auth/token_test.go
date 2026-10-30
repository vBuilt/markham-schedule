package auth

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestNewToken(t *testing.T) {
	raw, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) != 32 {
		t.Fatalf("raw decode: len=%d err=%v", len(b), err)
	}
	if !bytes.Equal(HashToken(raw), hash) {
		t.Error("hash mismatch")
	}
	raw2, _, _ := NewToken()
	if raw == raw2 {
		t.Error("tokens equal")
	}
}
