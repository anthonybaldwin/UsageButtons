package cookies

import (
	"errors"
	"testing"
)

func TestIsAllowed(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"claude.ai", true},
		{"www.claude.ai", true},
		{"api.claude.ai", true},
		{"CLAUDE.AI", true},
		{".claude.ai", true},
		{"  claude.ai  ", true},
		{"cursor.com", true},
		{"ollama.com", true},
		{"sub.deep.claude.ai", true},
		{"", false},
		{"   ", false},
		{"example.com", false},
		{"claudeaix.com", false},
		{"notclaude.ai", false},
		{"anthropic.com", false},
		{"ai", false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := IsAllowed(tc.host); got != tc.want {
				t.Fatalf("IsAllowed(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestURLAllowed(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://claude.ai/api/organizations", true},
		{"https://api.claude.ai/foo", true},
		{"https://ollama.com/settings", true},
		{"https://cursor.com/api/usage-summary", true},
		{"https://example.com/", false},
		{"http://claude.ai/", false},   // non-https rejected
		{"ftp://claude.ai/", false},     // non-https rejected
		{"://broken", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if got := URLAllowed(tc.url); got != tc.want {
				t.Fatalf("URLAllowed(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

func TestFetch_RejectsDisallowedOrigin(t *testing.T) {
	ctx := t.Context()
	_, err := Fetch(ctx, Request{URL: "https://evil.example.com/"})
	if !errors.Is(err, ErrOriginNotAllowed) {
		t.Fatalf("want ErrOriginNotAllowed, got %v", err)
	}
}

func TestFetch_RejectsNonHTTPS(t *testing.T) {
	ctx := t.Context()
	_, err := Fetch(ctx, Request{URL: "http://claude.ai/"})
	if !errors.Is(err, ErrOriginNotAllowed) {
		t.Fatalf("want ErrOriginNotAllowed (non-https), got %v", err)
	}
}

func TestFetch_RejectsEmptyURL(t *testing.T) {
	ctx := t.Context()
	_, err := Fetch(ctx, Request{URL: ""})
	if err == nil {
		t.Fatal("want error on empty URL")
	}
}
