package cookies

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarshalHostManifest_RoundTrip(t *testing.T) {
	in := HostManifest{
		Name:           HostName,
		Description:    "Usage Buttons cookie bridge",
		Path:           "/tmp/usagebuttons-native-host",
		Type:           "stdio",
		AllowedOrigins: []string{ExtensionOrigin("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
	}
	data, err := MarshalHostManifest(in)
	if err != nil {
		t.Fatal(err)
	}
	var out HostManifest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != HostName || out.Type != "stdio" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
	if !strings.Contains(string(data), "chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/") {
		t.Fatalf("allowed_origins missing in rendered JSON:\n%s", data)
	}
}

func TestMarshalHostManifest_DefaultsType(t *testing.T) {
	in := HostManifest{Name: "x", Path: "/y"}
	data, err := MarshalHostManifest(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"type": "stdio"`) {
		t.Fatalf("default type missing:\n%s", data)
	}
}

func TestMarshalHostManifest_RejectsEmptyNameOrPath(t *testing.T) {
	if _, err := MarshalHostManifest(HostManifest{Path: "/y"}); err == nil {
		t.Fatal("want error on empty name")
	}
	if _, err := MarshalHostManifest(HostManifest{Name: "x"}); err == nil {
		t.Fatal("want error on empty path")
	}
}

func TestExtensionOrigin(t *testing.T) {
	got := ExtensionOrigin("abcdef")
	if got != "chrome-extension://abcdef/" {
		t.Fatalf("got %q", got)
	}
}
