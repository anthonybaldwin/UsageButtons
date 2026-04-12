// Package update checks GitHub Releases for newer plugin versions.
package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthonybaldwin/UsageButtons/internal/httputil"
)

const (
	repo          = "anthonybaldwin/UsageButtons"
	checkInterval = 6 * time.Hour
	websiteURL    = "https://anthonybaldwin.github.io/UsageButtons/"
	repoURL       = "https://github.com/" + repo
)

// LogSink is wired by the plugin for observability.
var LogSink func(string)

func logf(format string, args ...any) {
	if LogSink != nil {
		LogSink(fmt.Sprintf("[update-checker] "+format, args...))
	}
}

type state struct {
	current         string
	latest          string
	updateAvailable bool
	lastCheckedAt   time.Time
}

var (
	mu sync.Mutex
	st = state{current: readCurrentVersion()}
)

// readCurrentVersion reads the 3-part semver from manifest.json.
// Tries process.execPath/../manifest.json first (production),
// then a dev-mode fallback.
func readCurrentVersion() string {
	exe, err := os.Executable()
	if err == nil {
		p := filepath.Join(filepath.Dir(exe), "..", "manifest.json")
		if v := readVersionFrom(p); v != "" {
			return v
		}
	}
	// Dev fallback
	wd, _ := os.Getwd()
	p := filepath.Join(wd, "io.github.anthonybaldwin.UsageButtons.sdPlugin", "manifest.json")
	if v := readVersionFrom(p); v != "" {
		return v
	}
	return "0.0.0"
}

func readVersionFrom(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	parts := strings.SplitN(m.Version, ".", 4)
	if len(parts) >= 3 {
		return strings.Join(parts[:3], ".")
	}
	return m.Version
}

func compareSemver(a, b string) int {
	pa := parseSemver(a)
	pb := parseSemver(b)
	for i := 0; i < 3; i++ {
		if pb[i] > pa[i] {
			return 1
		}
		if pb[i] < pa[i] {
			return -1
		}
	}
	return 0
}

func parseSemver(s string) [3]int {
	parts := strings.SplitN(s, ".", 4)
	var v [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		v[i], _ = strconv.Atoi(parts[i])
	}
	return v
}

type ghRelease struct {
	TagName string `json:"tag_name"`
}

func fetchLatestVersion() string {
	var rel ghRelease
	err := httputil.GetJSON(
		fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo),
		map[string]string{"Accept": "application/vnd.github+json"},
		10*time.Second, &rel,
	)
	if err != nil {
		logf("GitHub API error: %v", err)
		return ""
	}
	return strings.TrimPrefix(rel.TagName, "v")
}

// Check runs an update check if the cache has expired.
// Safe to call on every scheduler tick.
func Check() {
	mu.Lock()
	if time.Since(st.lastCheckedAt) < checkInterval {
		mu.Unlock()
		return
	}
	st.lastCheckedAt = time.Now()
	mu.Unlock()

	latest := fetchLatestVersion()
	if latest == "" {
		logf("check failed, keeping previous state")
		return
	}

	mu.Lock()
	st.latest = latest
	st.updateAvailable = compareSemver(st.current, latest) > 0
	mu.Unlock()

	if IsAvailable() {
		logf("update available: %s -> %s", st.current, latest)
	} else {
		logf("up to date (current=%s, latest=%s)", st.current, latest)
	}
}

// IsAvailable returns whether a newer version exists.
func IsAvailable() bool {
	mu.Lock()
	defer mu.Unlock()
	return st.updateAvailable
}

// LatestVersion returns the latest version string.
func LatestVersion() string {
	mu.Lock()
	defer mu.Unlock()
	return st.latest
}

// CurrentVersion returns the baked-in version.
func CurrentVersion() string {
	mu.Lock()
	defer mu.Unlock()
	return st.current
}

// URL returns the appropriate update URL based on install type.
// Dev installs (.git exists) → repo URL. Release bundles → website.
func URL() string {
	exe, err := os.Executable()
	if err != nil {
		return websiteURL
	}
	repoRoot := filepath.Join(filepath.Dir(exe), "..", "..")
	if _, err := os.Stat(filepath.Join(repoRoot, ".git")); err == nil {
		logf("install type: dev (git repo)")
		return repoURL
	}
	logf("install type: release bundle")
	return websiteURL
}
