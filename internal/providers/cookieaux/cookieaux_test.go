package cookieaux

import (
	"context"
	"strings"
	"testing"
)

func TestResolve_ManualPasteWins(t *testing.T) {
	r, ok := Resolve(context.Background(), "claude.ai", "sessionKey=abc; cf_clearance=xyz")
	if !ok {
		t.Fatal("want ok")
	}
	if r.Source != "manual" {
		t.Fatalf("source: %q", r.Source)
	}
	if r.Header != "sessionKey=abc; cf_clearance=xyz" {
		t.Fatalf("header: %q", r.Header)
	}
	if r.UserAgent != "" {
		t.Fatalf("manual should have no UA, got %q", r.UserAgent)
	}
}

func TestResolve_NoManualNoExtension(t *testing.T) {
	// HostAvailable returns false without a live host; resolve should
	// report not-ok so callers skip the request.
	_, ok := Resolve(context.Background(), "claude.ai", "")
	if ok {
		t.Fatal("want not ok when no manual paste and no extension")
	}
}

func TestHeaders_MergesExtras(t *testing.T) {
	r := Resolution{Header: "a=1", UserAgent: "UA", Source: "extension"}
	h := r.Headers(map[string]string{"Accept": "application/json"})
	if h["Cookie"] != "a=1" || h["User-Agent"] != "UA" || h["Accept"] != "application/json" {
		t.Fatalf("headers: %+v", h)
	}
}

func TestHeaders_ExtrasWinOnCollision(t *testing.T) {
	r := Resolution{Header: "a=1", UserAgent: "UA"}
	h := r.Headers(map[string]string{"User-Agent": "override"})
	if h["User-Agent"] != "override" {
		t.Fatalf("extras should win, got %q", h["User-Agent"])
	}
}

func TestHeaders_OmitsEmptyUA(t *testing.T) {
	r := Resolution{Header: "a=1", Source: "manual"}
	h := r.Headers(nil)
	if _, ok := h["User-Agent"]; ok {
		t.Fatalf("expected no UA, got %v", h)
	}
}

func TestMissingMessage_IncludesProvider(t *testing.T) {
	msg := MissingMessage("claude.ai")
	if !strings.Contains(msg, "claude.ai") {
		t.Fatalf("expected provider label in message, got %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "extension") {
		t.Fatalf("expected extension hint, got %q", msg)
	}
}
