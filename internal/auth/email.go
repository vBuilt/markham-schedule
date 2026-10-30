package auth

import "strings"

// NormalizeEmail trims and lowercases s and reports whether it looks like an
// email address.
func NormalizeEmail(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 254 || strings.ContainsAny(s, " \t\r\n") || strings.Count(s, "@") != 1 {
		return "", false
	}
	local, domain, _ := strings.Cut(s, "@")
	if local == "" || !strings.Contains(domain, ".") {
		return "", false
	}
	return s, true
}
