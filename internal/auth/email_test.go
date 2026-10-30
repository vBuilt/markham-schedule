package auth

import "testing"

func TestNormalizeEmail(t *testing.T) {
	got, ok := NormalizeEmail("  A@X.com ")
	if !ok || got != "a@x.com" {
		t.Fatalf("got %q %v", got, ok)
	}
	for _, s := range []string{"ax.com", "a@x", "a b@x.com", "@x.com", "a@@x.com", "a@b@x.com", ""} {
		if _, ok := NormalizeEmail(s); ok {
			t.Errorf("%q accepted", s)
		}
	}
}
