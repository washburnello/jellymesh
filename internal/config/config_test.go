package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func lookupFrom(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestDefaultsSplitTheListeners(t *testing.T) {
	config, err := FromLookup(lookupFrom(nil))
	if err != nil {
		t.Fatalf("load defaults: %v", err)
	}
	if config.FederationListenAddress != "0.0.0.0:8443" {
		t.Fatalf("unexpected federation listener: %s", config.FederationListenAddress)
	}
	// The relay and admin listeners must default to loopback. The relay serves
	// media to the local Jellyfin and must never be publicly reachable.
	for name, addr := range map[string]string{
		"relay": config.RelayListenAddress,
		"admin": config.AdminListenAddress,
	} {
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Fatalf("%s listener must default to loopback, got %s", name, addr)
		}
	}
	if config.RelayListenAddress == config.AdminListenAddress {
		t.Fatal("relay and admin listeners must not collide by default")
	}
}

func TestListenerCollisionIsRejected(t *testing.T) {
	_, err := FromLookup(lookupFrom(map[string]string{
		"JELLYMESH_FEDERATION_LISTEN_ADDR": "127.0.0.1:9000",
		"JELLYMESH_RELAY_LISTEN_ADDR":      "127.0.0.1:9000",
	}))
	if err == nil {
		t.Fatal("sharing an address between the federation and relay listeners must be rejected")
	}
}

func TestInvalidListenAddressIsRejected(t *testing.T) {
	if _, err := FromLookup(lookupFrom(map[string]string{"JELLYMESH_FEDERATION_LISTEN_ADDR": "not-an-address"})); err == nil {
		t.Fatal("expected an error for a malformed listen address")
	}
}

// A variable that is set but blank falls back to the default, consistent with
// every other field. Only a name that survives trimming is used verbatim.
func TestBlankNodeNameFallsBackToTheDefault(t *testing.T) {
	config, err := FromLookup(lookupFrom(map[string]string{"JELLYMESH_NODE_NAME": "   "}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.NodeName != "Jellymesh Node" {
		t.Fatalf("blank node name should fall back to the default, got %q", config.NodeName)
	}
	named, err := FromLookup(lookupFrom(map[string]string{"JELLYMESH_NODE_NAME": "  Cedar  "}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if named.NodeName != "Cedar" {
		t.Fatalf("node name should be trimmed, got %q", named.NodeName)
	}
}

func TestKeyMaterialDefaultsInsideTheDataDirectory(t *testing.T) {
	config, err := FromLookup(lookupFrom(map[string]string{"JELLYMESH_DATA_DIR": "/var/lib/jellymesh"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.NodeKeyPath != filepath.Join("/var/lib/jellymesh", "node.key") {
		t.Fatalf("unexpected node key path: %s", config.NodeKeyPath)
	}
	if config.NodeCertPath != filepath.Join("/var/lib/jellymesh", "node.crt") {
		t.Fatalf("unexpected node cert path: %s", config.NodeCertPath)
	}
}

func TestPathsAreAbsolute(t *testing.T) {
	config, err := FromLookup(lookupFrom(map[string]string{
		"JELLYMESH_DATA_DIR":       "./data",
		"JELLYMESH_GENERATED_ROOT": "./generated",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for name, path := range map[string]string{
		"data":      config.DataDirectory,
		"generated": config.GeneratedRootPath,
	} {
		if !filepath.IsAbs(path) {
			t.Fatalf("%s path should be absolute, got %s", name, path)
		}
	}
}

// Enabling federation without the material required to authenticate peers or
// read published libraries should fail at load rather than at first use.
func TestFederationRequiresHostnameAndJellyfinKey(t *testing.T) {
	base := map[string]string{"JELLYMESH_FEDERATION_ENABLED": "true"}
	if _, err := FromLookup(lookupFrom(base)); err == nil {
		t.Fatal("federation without a public hostname must be rejected")
	}

	withHost := map[string]string{
		"JELLYMESH_FEDERATION_ENABLED": "true",
		"JELLYMESH_PUBLIC_HOSTNAME":    "cedar.example.org",
	}
	if _, err := FromLookup(lookupFrom(withHost)); err == nil {
		t.Fatal("federation without a Jellyfin API key must be rejected")
	}

	complete := map[string]string{
		"JELLYMESH_FEDERATION_ENABLED": "true",
		"JELLYMESH_PUBLIC_HOSTNAME":    "cedar.example.org",
		"JELLYMESH_JELLYFIN_API_KEY":   "secret",
	}
	config, err := FromLookup(lookupFrom(complete))
	if err != nil {
		t.Fatalf("complete federation config should load: %v", err)
	}
	if !config.FederationEnabled {
		t.Fatal("federation should be enabled")
	}
}

func TestFederationDisabledByDefault(t *testing.T) {
	config, err := FromLookup(lookupFrom(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.FederationEnabled {
		t.Fatal("federation must be disabled by default")
	}
}

func TestInvalidFederationFlagIsRejected(t *testing.T) {
	if _, err := FromLookup(lookupFrom(map[string]string{"JELLYMESH_FEDERATION_ENABLED": "maybe"})); err == nil {
		t.Fatal("expected an error for a non-boolean federation flag")
	}
}

func TestJellyfinURLTrailingSlashIsTrimmed(t *testing.T) {
	config, err := FromLookup(lookupFrom(map[string]string{"JELLYMESH_JELLYFIN_URL": "http://host:8096/"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if config.JellyfinBaseURL != "http://host:8096" {
		t.Fatalf("trailing slash was not trimmed: %s", config.JellyfinBaseURL)
	}
}
