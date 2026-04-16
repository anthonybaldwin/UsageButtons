package cookies

import (
	"encoding/json"
	"fmt"
	"strings"
)

// HostManifest is the JSON shape Chrome expects for a native-messaging
// host manifest file. See
// https://developer.chrome.com/docs/apps/nativeMessaging/#native-messaging-host
type HostManifest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Path           string   `json:"path"`
	Type           string   `json:"type"` // always "stdio" for our use
	AllowedOrigins []string `json:"allowed_origins"`
}

// MarshalHostManifest renders the manifest JSON with stable indentation
// — Chrome is whitespace-tolerant but humans debug these files.
func MarshalHostManifest(m HostManifest) ([]byte, error) {
	if strings.TrimSpace(m.Name) == "" {
		return nil, fmt.Errorf("cookies: manifest Name is required")
	}
	if strings.TrimSpace(m.Path) == "" {
		return nil, fmt.Errorf("cookies: manifest Path is required")
	}
	if m.Type == "" {
		m.Type = "stdio"
	}
	return json.MarshalIndent(m, "", "  ")
}

// ExtensionOrigin formats an allowed-origin entry for a given extension
// ID, as Chrome expects: "chrome-extension://<id>/".
func ExtensionOrigin(id string) string {
	id = strings.TrimSpace(id)
	return "chrome-extension://" + id + "/"
}
