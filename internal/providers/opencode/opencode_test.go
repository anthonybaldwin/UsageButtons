package opencode

import (
	"strings"
	"testing"
	"time"
)

func TestParseSubscription_ActiveUsage(t *testing.T) {
	now := time.Now().UTC()
	text := `{"rollingUsage":{"usagePercent":42.5,"resetInSec":1800},"weeklyUsage":{"usagePercent":18,"resetInSec":345600}}`
	usage, err := parseSubscription(text, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.RollingUsagePercent != 42.5 {
		t.Errorf("RollingUsagePercent = %v, want 42.5", usage.RollingUsagePercent)
	}
	if usage.WeeklyUsagePercent != 18 {
		t.Errorf("WeeklyUsagePercent = %v, want 18", usage.WeeklyUsagePercent)
	}
	if usage.RollingResetInSec != 1800 {
		t.Errorf("RollingResetInSec = %v, want 1800", usage.RollingResetInSec)
	}
	if usage.WeeklyResetInSec != 345600 {
		t.Errorf("WeeklyResetInSec = %v, want 345600", usage.WeeklyResetInSec)
	}
}

func TestParseSubscription_EmptyWorkspace_KeysShape(t *testing.T) {
	now := time.Now().UTC()
	text := `;0x000000d7;
((self.$R = self.$R || {})["server-fn:0"] = [],
($R => $R[0] = {
    usage: $R[1] = [],
    keys: $R[2] = [$R[3] = {id: "key_X", displayName: "x@y.z", deleted: !1}]
})($R["server-fn:0"]))`
	usage, err := parseSubscription(text, now)
	if err != nil {
		t.Fatalf("expected no error for empty-workspace response, got: %v", err)
	}
	if usage.RollingUsagePercent != 0 || usage.WeeklyUsagePercent != 0 {
		t.Errorf("expected zero percents, got rolling=%v weekly=%v",
			usage.RollingUsagePercent, usage.WeeklyUsagePercent)
	}
}

func TestParseSubscription_NoSubscription_BillingShape(t *testing.T) {
	now := time.Now().UTC()
	text := `;0x000001f9;
((self.$R = self.$R || {})["server-fn:6"] = [],
($R => $R[0] = {
    customerID: null, balance: 0,
    monthlyLimit: null, monthlyUsage: null,
    subscription: null, subscriptionID: null, subscriptionPlan: null
})($R["server-fn:6"]))`
	usage, err := parseSubscription(text, now)
	if err != nil {
		t.Fatalf("expected no error for no-subscription response, got: %v", err)
	}
	if usage.RollingUsagePercent != 0 || usage.WeeklyUsagePercent != 0 {
		t.Errorf("expected zero percents, got rolling=%v weekly=%v",
			usage.RollingUsagePercent, usage.WeeklyUsagePercent)
	}
}

func TestParseSubscription_NullPayload_SolidWrapped(t *testing.T) {
	now := time.Now().UTC()
	// Real shape captured from an unsubscribed account hitting the
	// subscription server-fn — entire payload resolves to null after the
	// Solid SSR wrapper. 93 bytes total, no usage fields anywhere.
	text := `;0x00000051;((self.$R=self.$R||{})["server-fn:00000000-0000-4000-8000-000000000000"]=[],null)`
	usage, err := parseSubscription(text, now)
	if err != nil {
		t.Fatalf("expected no error for Solid-wrapped null payload, got: %v", err)
	}
	if usage.RollingUsagePercent != 0 || usage.WeeklyUsagePercent != 0 {
		t.Errorf("expected zero percents, got rolling=%v weekly=%v",
			usage.RollingUsagePercent, usage.WeeklyUsagePercent)
	}
}

func TestParseSubscription_NullUsageFields(t *testing.T) {
	now := time.Now().UTC()
	// Subscription endpoint shape we believe applies to unsubscribed
	// accounts: schema keys present, values null, no usagePercent anywhere.
	text := `;0x000000aa;
((self.$R = self.$R || {})["server-fn:3"] = [],
($R => $R[0] = {
    rollingUsage: null,
    weeklyUsage: null
})($R["server-fn:3"]))`
	usage, err := parseSubscription(text, now)
	if err != nil {
		t.Fatalf("expected no error for null-usage response, got: %v", err)
	}
	if usage.RollingUsagePercent != 0 || usage.WeeklyUsagePercent != 0 {
		t.Errorf("expected zero percents, got rolling=%v weekly=%v",
			usage.RollingUsagePercent, usage.WeeklyUsagePercent)
	}
}

func TestParseSubscription_NoSubscription_Minified(t *testing.T) {
	now := time.Now().UTC()
	// Same shape as the billing response but without spaces after colons —
	// what we'd see if OpenCode minifies the Solid SSR output.
	text := `;0x000001f9;((self.$R=self.$R||{})["server-fn:6"]=[],($R=>$R[0]={customerID:null,balance:0,monthlyLimit:null,monthlyUsage:null,subscription:null,subscriptionID:null,subscriptionPlan:null})($R["server-fn:6"]))`
	usage, err := parseSubscription(text, now)
	if err != nil {
		t.Fatalf("expected no error for minified no-subscription response, got: %v", err)
	}
	if usage.RollingUsagePercent != 0 || usage.WeeklyUsagePercent != 0 {
		t.Errorf("expected zero percents, got rolling=%v weekly=%v",
			usage.RollingUsagePercent, usage.WeeklyUsagePercent)
	}
}

func TestParseSubscription_BrokenResponse_StillErrors(t *testing.T) {
	now := time.Now().UTC()
	// No Solid markers, no empty-state markers — looks like a genuine
	// schema regression we want to surface, not silently zero.
	text := `<html><body>Internal error</body></html>`
	_, err := parseSubscription(text, now)
	if err == nil {
		t.Fatal("expected error for unrecognized response, got nil")
	}
	if !strings.Contains(err.Error(), "missing usage fields") {
		t.Errorf("expected 'missing usage fields' error, got: %v", err)
	}
}
