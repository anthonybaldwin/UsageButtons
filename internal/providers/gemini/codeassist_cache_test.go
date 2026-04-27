package gemini

import "testing"

func TestCodeAssistCache_EmptyEmailNeverCached(t *testing.T) {
	resetCodeAssistCache()
	rememberCodeAssist("", codeAssistStatus{ProjectID: "proj"})
	if _, ok := cachedCodeAssist(""); ok {
		t.Error("empty email must not be cached")
	}
}

func TestCodeAssistCache_StoresAndRetrieves(t *testing.T) {
	resetCodeAssistCache()
	want := codeAssistStatus{Tier: "free-tier", ProjectID: "proj-123"}
	rememberCodeAssist("user@example.com", want)
	got, ok := cachedCodeAssist("user@example.com")
	if !ok {
		t.Fatal("expected cache hit after remember")
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestCodeAssistCache_PartialResultNotCached(t *testing.T) {
	// loadCodeAssist + discoverGeminiProjectID can both fail, in which
	// case ProjectID stays empty. Don't cache that — we want to retry
	// next tick.
	resetCodeAssistCache()
	rememberCodeAssist("user@example.com", codeAssistStatus{Tier: "x"})
	if _, ok := cachedCodeAssist("user@example.com"); ok {
		t.Error("status without ProjectID must not be cached")
	}
}

func TestCodeAssistCache_AccountSwitchInvalidates(t *testing.T) {
	resetCodeAssistCache()
	rememberCodeAssist("a@example.com", codeAssistStatus{ProjectID: "proj-a"})
	rememberCodeAssist("b@example.com", codeAssistStatus{ProjectID: "proj-b"})
	gotA, _ := cachedCodeAssist("a@example.com")
	gotB, _ := cachedCodeAssist("b@example.com")
	if gotA.ProjectID != "proj-a" || gotB.ProjectID != "proj-b" {
		t.Errorf("entries must not collide: got A=%q B=%q", gotA.ProjectID, gotB.ProjectID)
	}
}
