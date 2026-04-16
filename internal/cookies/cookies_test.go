package cookies

import (
	"context"
	"errors"
	"testing"
)

func TestIsAllowed(t *testing.T) {
	cases := []struct {
		domain string
		want   bool
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
		{"claudeaix.com", false}, // not a subdomain of claude.ai
		{"notclaude.ai", false},  // prefix boundary check
		{"anthropic.com", false},
		{"ai", false},
	}
	for _, tc := range cases {
		t.Run(tc.domain, func(t *testing.T) {
			if got := IsAllowed(tc.domain); got != tc.want {
				t.Fatalf("IsAllowed(%q) = %v, want %v", tc.domain, got, tc.want)
			}
		})
	}
}

func TestQueryValidate(t *testing.T) {
	t.Run("allowed", func(t *testing.T) {
		if err := (Query{Domain: "claude.ai"}).Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("allowed-subdomain", func(t *testing.T) {
		if err := (Query{Domain: "api.claude.ai"}).Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("disallowed", func(t *testing.T) {
		err := (Query{Domain: "evil.example.com"}).Validate()
		if !errors.Is(err, ErrDomainNotAllowed) {
			t.Fatalf("want ErrDomainNotAllowed, got %v", err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		err := (Query{Domain: ""}).Validate()
		if err == nil {
			t.Fatal("want error for empty domain")
		}
	})
}

func TestGet_RejectsDisallowedDomain(t *testing.T) {
	_, err := Get(context.Background(), Query{Domain: "evil.example.com"})
	if !errors.Is(err, ErrDomainNotAllowed) {
		t.Fatalf("want ErrDomainNotAllowed, got %v", err)
	}
}

func TestHeader_ComposesFromBundle(t *testing.T) {
	prev := dispatchGet
	t.Cleanup(func() { dispatchGet = prev })
	dispatchGet = func(ctx context.Context, q Query) (Bundle, error) {
		return Bundle{
			Cookies: []Cookie{
				{Name: "sessionKey", Value: "abc"},
				{Name: "cf_clearance", Value: "xyz"},
			},
			UserAgent: "Mozilla/5.0 test",
		}, nil
	}
	h, ua, err := Header(context.Background(), Query{Domain: "claude.ai"})
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	const want = "sessionKey=abc; cf_clearance=xyz"
	if h != want {
		t.Fatalf("header: got %q, want %q", h, want)
	}
	if ua != "Mozilla/5.0 test" {
		t.Fatalf("ua: got %q", ua)
	}
}

func TestHeader_NoCookies(t *testing.T) {
	prev := dispatchGet
	t.Cleanup(func() { dispatchGet = prev })
	dispatchGet = func(ctx context.Context, q Query) (Bundle, error) {
		return Bundle{UserAgent: "Mozilla/5.0 test"}, nil
	}
	_, ua, err := Header(context.Background(), Query{Domain: "claude.ai"})
	if !errors.Is(err, ErrNoCookies) {
		t.Fatalf("want ErrNoCookies, got %v", err)
	}
	if ua != "Mozilla/5.0 test" {
		t.Fatalf("ua should still propagate, got %q", ua)
	}
}

