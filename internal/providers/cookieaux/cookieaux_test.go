package cookieaux

import (
	"strings"
	"testing"
)

func TestMissingMessage_IncludesProvider(t *testing.T) {
	msg := MissingMessage("claude.ai")
	if !strings.Contains(msg, "claude.ai") {
		t.Fatalf("want provider label in %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "extension") {
		t.Fatalf("want extension hint in %q", msg)
	}
}

func TestStaleMessage_IncludesProvider(t *testing.T) {
	msg := StaleMessage("cursor.com")
	if !strings.Contains(msg, "cursor.com") {
		t.Fatalf("want provider label in %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "sign in") {
		t.Fatalf("want recovery hint in %q", msg)
	}
}
