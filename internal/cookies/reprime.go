package cookies

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// reprimeMinInterval is the minimum gap between two Reprime calls for
// the same URL. Belt-and-suspenders alongside the extension's own
// limiter: this gate avoids hitting the IPC at all when nothing has
// changed, and survives extension service-worker suspensions where the
// extension's in-memory map gets wiped.
//
// Exposed as a var so tests can shrink it.
var reprimeMinInterval = 30 * time.Minute

// reprimeBudget bounds how long the plugin waits for the extension's
// reply. The extension sleeps ~8s while DataDome's JS runs in the
// background tab; 30s gives it ample headroom.
var reprimeBudget = 30 * time.Second

// reprimeMu guards lastReprimeAt.
var reprimeMu sync.Mutex

// lastReprimeAt records the most recent Reprime time per URL.
var lastReprimeAt = map[string]time.Time{}

// ErrReprimeRateLimited is returned when Reprime is called for a URL
// whose Go-side cooldown has not yet expired. Not an error condition
// for callers — they typically discard it.
var ErrReprimeRateLimited = errors.New("cookies: reprime rate-limited")

// Reprime asks the extension to refresh the host's cookies by loading
// the dashboard URL in a real browser tab — either by reloading an
// existing tab or by opening + closing a hidden one. Used to recover
// from anti-bot challenges (e.g. DataDome on portal.nousresearch.com)
// without user interaction.
//
// Best-effort. Returns ErrReprimeRateLimited when the per-URL cooldown
// has not expired, ErrHostUnavailable when the native host or extension
// isn't connected, or a generic error for other failures. Callers
// typically log and ignore — the next provider fetch tick exposes
// whether the reprime worked.
func Reprime(ctx context.Context, dashboardURL string) error {
	if !URLAllowed(dashboardURL) {
		return fmt.Errorf("%w: %q", ErrOriginNotAllowed, dashboardURL)
	}

	reprimeMu.Lock()
	last, seen := lastReprimeAt[dashboardURL]
	now := time.Now()
	if seen && now.Sub(last) < reprimeMinInterval {
		reprimeMu.Unlock()
		return ErrReprimeRateLimited
	}
	// Record the attempt before issuing IPC so concurrent callers don't
	// pile up on the same URL while the extension is mid-reload.
	lastReprimeAt[dashboardURL] = now
	reprimeMu.Unlock()

	return dispatchReprime(ctx, dashboardURL)
}

// dispatchReprime is overridden in ipc.go init(); the default keeps the
// package compilable when the IPC transport hasn't been linked in.
var dispatchReprime = func(ctx context.Context, dashboardURL string) error {
	_ = ctx
	_ = dashboardURL
	return ErrHostUnavailable
}

// resetReprimeStateForTest clears the rate-limit map. Test-only.
func resetReprimeStateForTest() {
	reprimeMu.Lock()
	defer reprimeMu.Unlock()
	lastReprimeAt = map[string]time.Time{}
}
