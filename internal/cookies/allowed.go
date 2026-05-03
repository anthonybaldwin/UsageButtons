package cookies

import (
	"net/url"
	"strings"
)

// Allowed is the compile-time allowlist of domains the companion
// extension will fetch for. The extension mirrors this list in its
// service worker and in manifest.json host_permissions. Changes here
// require a coordinated extension release.
var Allowed = []string{
	"abacus.ai",
	"alibabacloud.com",
	"aliyun.com",
	"claude.ai",
	"cursor.com",
	"factory.ai",
	"ollama.com",
	"chatgpt.com",
	"augmentcode.com",
	"ampcode.com",
	"perplexity.ai",
	"grok.com",
	"nousresearch.com",
	"opencode.ai",
	"kimi.com",
	"minimax.io",
	"minimaxi.com",
	"mistral.ai",
	// deepseek.com covers platform.deepseek.com (web-token bridge for
	// usage/cost + usage/amount endpoints — Authorization Bearer token
	// is read from localStorage["userToken"] in the platform tab by the
	// extension's service worker; see chrome-extension/service-worker.js).
	"deepseek.com",
}

// IsAllowed reports whether host is covered by the allowlist. A host
// matches if it equals an allowlist entry or is a subdomain of one.
// Leading dots and case are tolerated.
func IsAllowed(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimPrefix(h, ".")
	if h == "" {
		return false
	}
	for _, a := range Allowed {
		if h == a || strings.HasSuffix(h, "."+a) {
			return true
		}
	}
	return false
}

// URLAllowed parses rawURL and reports whether its host is in the
// allowlist and uses an https scheme. Malformed URLs and non-https
// schemes are rejected.
func URLAllowed(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "https" {
		return false
	}
	return IsAllowed(u.Hostname())
}
