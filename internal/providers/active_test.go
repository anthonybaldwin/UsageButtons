package providers

import (
	"reflect"
	"testing"
	"time"
)

func TestActive_EmptyRegistryReturnsNil(t *testing.T) {
	ResetActiveRegistry()
	if got := ActiveFor("anything"); got != nil {
		t.Errorf("expected nil for unknown provider, got %v", got)
	}
}

func TestActive_MarkActive_AppearsInResult(t *testing.T) {
	ResetActiveRegistry()
	MarkActive("perplexity", "balance")
	got := ActiveFor("perplexity")
	want := []string{"balance"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestActive_DedupedAndSorted(t *testing.T) {
	ResetActiveRegistry()
	MarkActive("gemini", "weekly-percent")
	MarkActive("gemini", "session-percent")
	MarkActive("gemini", "session-percent") // dup — same metric
	got := ActiveFor("gemini")
	want := []string{"session-percent", "weekly-percent"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestActive_ProviderIDIsolation(t *testing.T) {
	ResetActiveRegistry()
	MarkActive("a", "x")
	MarkActive("b", "y")
	if got := ActiveFor("a"); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("provider a: got %v, want [x]", got)
	}
	if got := ActiveFor("b"); !reflect.DeepEqual(got, []string{"y"}) {
		t.Errorf("provider b: got %v, want [y]", got)
	}
}

func TestActive_RefCountedRemoval(t *testing.T) {
	// Two buttons bound to the same metric — one disappear keeps the
	// metric active because the other button still binds it. Only
	// disappear of the last bound button starts the grace window.
	ResetActiveRegistry()
	MarkActive("p", "m")
	MarkActive("p", "m")
	MarkInactive("p", "m")
	if got := ActiveFor("p"); !reflect.DeepEqual(got, []string{"m"}) {
		t.Errorf("after one of two disappear: got %v, want [m]", got)
	}
	MarkInactive("p", "m")
	// In grace window — still active.
	if got := ActiveFor("p"); !reflect.DeepEqual(got, []string{"m"}) {
		t.Errorf("inside grace window: got %v, want [m]", got)
	}
}

func TestActive_GraceWindowExpiry(t *testing.T) {
	ResetActiveRegistry()
	saved := ActiveGracePeriod
	ActiveGracePeriod = 10 * time.Millisecond
	defer func() { ActiveGracePeriod = saved }()

	MarkActive("p", "m")
	MarkInactive("p", "m")
	if got := ActiveFor("p"); !reflect.DeepEqual(got, []string{"m"}) {
		t.Fatalf("inside grace: got %v, want [m]", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := ActiveFor("p"); got != nil {
		t.Errorf("after grace expiry: got %v, want nil", got)
	}
}

func TestActive_RebindCancelsGrace(t *testing.T) {
	// A profile-switch (disappear → appear within the grace window)
	// must not flap the active set: the metric stays registered as
	// active throughout, no expiration is observable.
	ResetActiveRegistry()
	saved := ActiveGracePeriod
	ActiveGracePeriod = 50 * time.Millisecond
	defer func() { ActiveGracePeriod = saved }()

	MarkActive("p", "m")
	MarkInactive("p", "m")
	MarkActive("p", "m") // re-bind inside grace window
	time.Sleep(80 * time.Millisecond)
	if got := ActiveFor("p"); !reflect.DeepEqual(got, []string{"m"}) {
		t.Errorf("rebind should have cancelled grace: got %v, want [m]", got)
	}
}

func TestActive_EmptyArgsAreNoOps(t *testing.T) {
	ResetActiveRegistry()
	MarkActive("", "m")    // should not register anything
	MarkActive("p", "")    // should not register anything
	MarkInactive("", "m")  // no-op
	MarkInactive("p", "")  // no-op
	if got := ActiveFor("p"); got != nil {
		t.Errorf("empty-arg calls should not populate registry, got %v", got)
	}
}

func TestActive_InactiveWithoutActiveIsNoOp(t *testing.T) {
	// MarkInactive on a never-marked pair must not panic and must not
	// pollute the registry.
	ResetActiveRegistry()
	MarkInactive("p", "m")
	if got := ActiveFor("p"); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}
