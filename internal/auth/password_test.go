package auth

import (
	"strings"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected PHC prefix: %s", h)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Error("correct password rejected")
	}
	if CheckPassword(h, "wrong") {
		t.Error("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Error("two hashes identical")
	}
	if CheckPassword("garbage", h) || CheckPassword("garbage", "x") || CheckPassword("", "") {
		t.Error("malformed accepted")
	}
}
