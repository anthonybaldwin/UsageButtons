package providers

import (
	"os"
	"path/filepath"
	"strings"
)

// claudeCredPath returns the filesystem path to the Claude OAuth credentials.
func claudeCredPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", ".credentials.json")
}

// codexCredPath returns the filesystem path to the Codex OAuth credentials,
// honoring the CODEX_HOME override when set.
func codexCredPath() string {
	if ch := strings.TrimSpace(os.Getenv("CODEX_HOME")); ch != "" {
		return filepath.Join(ch, "auth.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "auth.json")
}

// codexConfigPath returns the Codex config.toml path.
func codexConfigPath() string {
	if ch := strings.TrimSpace(os.Getenv("CODEX_HOME")); ch != "" {
		return filepath.Join(ch, "config.toml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

// geminiCredPath mirrors Gemini CLI's OAuth credentials path.
func geminiCredPath() string {
	configDir := geminiConfigDir()
	if configDir == "" {
		return ""
	}
	return filepath.Join(configDir, "oauth_creds.json")
}

// geminiConfigDir mirrors Gemini CLI's config directory selection.
func geminiConfigDir() string {
	for _, name := range []string{"GEMINI_CONFIG_DIR", "GEMINI_CONFIG_HOME"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return filepath.Clean(value)
		}
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini")
}

// vertexAICredPath mirrors gcloud's ADC credentials path.
func vertexAICredPath() string {
	configDir := vertexAIConfigDir()
	if configDir == "" {
		return ""
	}
	return filepath.Join(configDir, "application_default_credentials.json")
}

// vertexAIConfigDir mirrors gcloud's config directory selection.
func vertexAIConfigDir() string {
	if value := strings.TrimSpace(os.Getenv("CLOUDSDK_CONFIG")); value != "" {
		return filepath.Clean(value)
	}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		path := filepath.Join(appData, "gcloud")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "gcloud")
}

// CopilotHostsPath returns the GitHub Copilot hosts.json path.
func CopilotHostsPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "github-copilot", "hosts.json")
}

// CopilotAppsPath returns the GitHub Copilot apps.json path.
func CopilotAppsPath() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "github-copilot", "apps.json")
}
