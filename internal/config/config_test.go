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
func TestFederationRequiresHostnameAndServiceUser(t *testing.T) {
	base := map[string]string{"JELLYMESH_FEDERATION_ENABLED": "true"}
	if _, err := FromLookup(lookupFrom(base)); err == nil {
		t.Fatal("federation without a public hostname must be rejected")
	}

	withHost := map[string]string{
		"JELLYMESH_FEDERATION_ENABLED": "true",
		"JELLYMESH_PUBLIC_HOSTNAME":    "cedar.example.org",
	}
	if _, err := FromLookup(lookupFrom(withHost)); err == nil {
		t.Fatal("federation without a Jellyfin service user must be rejected")
	}

	complete := map[string]string{
		"JELLYMESH_FEDERATION_ENABLED":  "true",
		"JELLYMESH_PUBLIC_HOSTNAME":     "cedar.example.org",
		"JELLYMESH_JELLYFIN_USER":       "jellymesh",
		"JELLYMESH_JELLYFIN_PASSWORD":   " a password with spaces ",
		"JELLYMESH_PROTECTED_LIBRARIES": "abc, def ,",
	}
	config, err := FromLookup(lookupFrom(complete))
	if err != nil {
		t.Fatalf("complete federation config should load: %v", err)
	}
	if !config.FederationEnabled || config.JellyfinPassword != " a password with spaces " {
		t.Fatalf("config = %+v", config)
	}
	if len(config.ProtectedLibraries) != 2 || config.ProtectedLibraries[1] != "def" {
		t.Fatalf("protected libraries = %v", config.ProtectedLibraries)
	}
}

// A-8: an administrator API key is refused outright, and the service user's
// name and password come together.
func TestAnAdministratorKeyIsRefused(t *testing.T) {
	if _, err := FromLookup(lookupFrom(map[string]string{"JELLYMESH_JELLYFIN_API_KEY": "admin-key"})); err == nil {
		t.Fatal("an administrator API key must be refused")
	}
	if _, err := FromLookup(lookupFrom(map[string]string{"JELLYMESH_JELLYFIN_USER": "jellymesh"})); err == nil {
		t.Fatal("a user without a password must be refused")
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

func TestPublicAddress(t *testing.T) {
	cases := map[string]Config{
		"cedar.example.org:8443": {PublicHostname: "cedar.example.org", FederationListenAddress: "0.0.0.0:8443"},
		"cedar.example.org:443":  {PublicHostname: "cedar.example.org:443", FederationListenAddress: "0.0.0.0:8443"},
		"":                       {FederationListenAddress: "0.0.0.0:8443"},
	}
	for want, cfg := range cases {
		if got := cfg.PublicAddress(); got != want {
			t.Errorf("PublicAddress(%+v) = %q, want %q", cfg, got, want)
		}
	}
}

// C-OP-6: the admin API is loopback-only.
func TestTheAdminListenerMustBeLoopback(t *testing.T) {
	for address, ok := range map[string]bool{
		"127.0.0.1:8091": true, "[::1]:8091": true, "localhost:8091": true,
		"0.0.0.0:8091": false, "192.168.1.10:8091": false, ":8091": false,
	} {
		_, err := FromLookup(func(key string) (string, bool) {
			if key == "JELLYMESH_ADMIN_LISTEN_ADDR" {
				return address, true
			}
			return "", false
		})
		if (err == nil) != ok {
			t.Errorf("admin address %q: error = %v, want accepted=%v", address, err, ok)
		}
	}
}

// C-PR-5: the relay URL a .strm names is a plain local URL, with no room for
// a credential, and the relay's allowed clients and upload ceiling have
// defaults.
func TestRelayConfiguration(t *testing.T) {
	cfg, err := FromLookup(lookupFrom(map[string]string{}))
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if cfg.RelayURL != "http://127.0.0.1:8090" || cfg.RelayAllowedClients == "" || cfg.UploadCeiling != 2_500_000 {
		t.Fatalf("defaults = %q, %q, %d", cfg.RelayURL, cfg.RelayAllowedClients, cfg.UploadCeiling)
	}
	for _, bad := range []string{"ftp://x:1", "http://user:pass@127.0.0.1:8090", "http://127.0.0.1:8090/?token=1", "http://127.0.0.1:8090/path", "127.0.0.1:8090"} {
		if _, err := FromLookup(lookupFrom(map[string]string{"JELLYMESH_RELAY_URL": bad})); err == nil {
			t.Errorf("relay URL %q should be refused", bad)
		}
	}
	if _, err := FromLookup(lookupFrom(map[string]string{"JELLYMESH_UPLOAD_CEILING_MBPS": "-1"})); err == nil {
		t.Error("a negative ceiling should be refused")
	}
	if cfg.HeadCacheBytes != 1024<<20 {
		t.Errorf("the head cache should default to 1 GiB, got %d", cfg.HeadCacheBytes)
	}
}
