package cookieaux

import (
	"context"
	"strings"
	"testing"
)

func TestFetcher_AvailableWithManualCookie(t *testing.T) {
	f := Fetcher{Domain: "claude.ai", ManualCookie: "sessionKey=abc"}
	if !f.Available(context.Background()) {
		t.Fatal("want available with manual cookie")
	}
}

func TestFetcher_NoManualNoExtensionUnavailable(t *testing.T) {
	f := Fetcher{Domain: "claude.ai"}
	if f.Available(context.Background()) {
		t.Fatal("want unavailable with neither")
	}
	if src := f.Source(context.Background()); src != SourceNone {
		t.Fatalf("source: %q", src)
	}
}

func TestFetcher_SourceManualWhenExtensionAbsent(t *testing.T) {
	f := Fetcher{Domain: "claude.ai", ManualCookie: "a=1"}
	if src := f.Source(context.Background()); src != SourceManual {
		t.Fatalf("source: %q", src)
	}
}

func TestMergeManualHeaders_CookieSet(t *testing.T) {
	got := mergeManualHeaders(map[string]string{"Accept": "application/json"}, "a=1")
	if got["Cookie"] != "a=1" || got["Accept"] != "application/json" {
		t.Fatalf("merged: %+v", got)
	}
}

func TestMergeManualHeaders_CookieOverridesExtra(t *testing.T) {
	got := mergeManualHeaders(map[string]string{"Cookie": "old"}, "new")
	if got["Cookie"] != "new" {
		t.Fatalf("cookie should win from manual arg, got %q", got["Cookie"])
	}
}

func TestMissingMessage_IncludesProvider(t *testing.T) {
	msg := MissingMessage("claude.ai")
	if !strings.Contains(msg, "claude.ai") {
		t.Fatalf("want provider label in %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "extension") {
		t.Fatalf("want extension hint in %q", msg)
	}
}

func TestStaleMessage_DistinguishesSource(t *testing.T) {
	ext := StaleMessage(SourceExtension, "cursor.com")
	man := StaleMessage(SourceManual, "cursor.com")
	if !strings.Contains(strings.ToLower(ext), "browser") {
		t.Fatalf("extension hint: %q", ext)
	}
	if !strings.Contains(strings.ToLower(man), "paste") {
		t.Fatalf("manual hint: %q", man)
	}
}
