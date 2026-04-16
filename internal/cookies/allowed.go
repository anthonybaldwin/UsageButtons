package cookies

import "strings"

// Allowed is the compile-time allowlist of cookie domains this package
// can query. The companion Chrome extension mirrors this list in its
// service worker and in manifest.json host_permissions. Changes here
// require a coordinated extension release.
var Allowed = []string{
	"claude.ai",
	"cursor.com",
	"ollama.com",
}

// IsAllowed reports whether domain is covered by the allowlist. A
// domain matches if it equals an allowlist entry or is a subdomain of
// one. Leading dots and case are tolerated.
func IsAllowed(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	d = strings.TrimPrefix(d, ".")
	if d == "" {
		return false
	}
	for _, a := range Allowed {
		if d == a || strings.HasSuffix(d, "."+a) {
			return true
		}
	}
	return false
}
