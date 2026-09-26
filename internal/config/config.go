// Package config loads and validates the Jellymesh Service configuration.
//
// The listener split is deliberate. The federation listener is public and
// authenticates peers with mutual TLS. The relay listener is local-only, serves
// generated media references to the co-located Jellyfin, and must never be
// reachable from outside the host. Keeping them as separate sockets means the
// federation listener's configuration cannot also expose the relay. Nothing
// here yet stops the relay itself being bound to a public address, and a
// loopback bind alone is not sufficient once Jellyfin runs in its own network
// namespace; both are C-PR-5.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	// FederationListenAddress is the public mTLS listener for peer traffic.
	FederationListenAddress string
	// RelayListenAddress serves media to the local Jellyfin only.
	RelayListenAddress string
	// AdminListenAddress serves the local operator interface.
	AdminListenAddress string

	// PublicHostname is the name peers use to reach this node. It is recorded
	// for certificate issuance and peer directory display. It is never used for
	// authorization: peers are authorized by node key fingerprint.
	PublicHostname string

	// NodeKeyPath is the long-term Ed25519 identity key. The same key backs the
	// mTLS client and server certificates.
	NodeKeyPath string
	// NodeCertPath is the certificate presented to peers.
	NodeCertPath string

	// JellyfinBaseURL and JellyfinAPIKey address the co-located Jellyfin.
	JellyfinBaseURL string
	JellyfinAPIKey  string

	DataDirectory     string
	GeneratedRootPath string
	NodeName          string
	FederationEnabled bool
}

func Load() (Config, error) {
	return FromLookup(os.LookupEnv)
}

func FromLookup(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{}
	var err error

	// Listeners. The federation listener defaults to all interfaces because it
	// must be reachable by peers; the relay and admin listeners default to
	// loopback because they must not be.
	if cfg.FederationListenAddress, err = address(lookup, "JELLYMESH_FEDERATION_LISTEN_ADDR", "0.0.0.0:8443"); err != nil {
		return Config{}, err
	}
	if cfg.RelayListenAddress, err = address(lookup, "JELLYMESH_RELAY_LISTEN_ADDR", "127.0.0.1:8090"); err != nil {
		return Config{}, err
	}
	if cfg.AdminListenAddress, err = address(lookup, "JELLYMESH_ADMIN_LISTEN_ADDR", "127.0.0.1:8091"); err != nil {
		return Config{}, err
	}
	if cfg.FederationListenAddress == cfg.RelayListenAddress {
		return Config{}, fmt.Errorf("the federation and relay listeners must not share an address")
	}
	if cfg.FederationListenAddress == cfg.AdminListenAddress {
		return Config{}, fmt.Errorf("the federation and admin listeners must not share an address")
	}
	// The admin API administers the node. It is served only on loopback, and
	// reached with the CLI on the same host (inside the container, in a
	// container deployment); the bearer token is a second barrier, not the
	// only one.
	if !isLoopback(cfg.AdminListenAddress) {
		return Config{}, fmt.Errorf("JELLYMESH_ADMIN_LISTEN_ADDR must be a loopback address, not %q", cfg.AdminListenAddress)
	}

	cfg.NodeName = strings.TrimSpace(valueOrDefault(lookup, "JELLYMESH_NODE_NAME", "Jellymesh Node"))
	if cfg.NodeName == "" {
		return Config{}, fmt.Errorf("JELLYMESH_NODE_NAME must not be empty")
	}

	cfg.PublicHostname = strings.TrimSpace(valueOrDefault(lookup, "JELLYMESH_PUBLIC_HOSTNAME", ""))

	if cfg.DataDirectory, err = absolutePath(lookup, "JELLYMESH_DATA_DIR", "./data"); err != nil {
		return Config{}, err
	}
	if cfg.GeneratedRootPath, err = absolutePath(lookup, "JELLYMESH_GENERATED_ROOT", "./generated"); err != nil {
		return Config{}, err
	}
	if cfg.NodeKeyPath, err = absolutePath(lookup, "JELLYMESH_NODE_KEY", filepath.Join(cfg.DataDirectory, "node.key")); err != nil {
		return Config{}, err
	}
	if cfg.NodeCertPath, err = absolutePath(lookup, "JELLYMESH_NODE_CERT", filepath.Join(cfg.DataDirectory, "node.crt")); err != nil {
		return Config{}, err
	}

	cfg.JellyfinBaseURL = strings.TrimRight(strings.TrimSpace(valueOrDefault(lookup, "JELLYMESH_JELLYFIN_URL", "http://127.0.0.1:8096")), "/")
	cfg.JellyfinAPIKey = strings.TrimSpace(valueOrDefault(lookup, "JELLYMESH_JELLYFIN_API_KEY", ""))

	federationEnabledValue := valueOrDefault(lookup, "JELLYMESH_FEDERATION_ENABLED", "false")
	if cfg.FederationEnabled, err = strconv.ParseBool(federationEnabledValue); err != nil {
		return Config{}, fmt.Errorf("invalid JELLYMESH_FEDERATION_ENABLED: %w", err)
	}

	// Federation cannot be switched on without the material needed to
	// authenticate peers or to read the libraries being published. Failing here
	// is better than starting a node that silently cannot federate.
	if cfg.FederationEnabled {
		if cfg.PublicHostname == "" {
			return Config{}, fmt.Errorf("JELLYMESH_PUBLIC_HOSTNAME is required when federation is enabled")
		}
		if cfg.JellyfinAPIKey == "" {
			return Config{}, fmt.Errorf("JELLYMESH_JELLYFIN_API_KEY is required when federation is enabled")
		}
	}

	return cfg, nil
}

func address(lookup func(string) (string, bool), key string, fallback string) (string, error) {
	value := valueOrDefault(lookup, key, fallback)
	if _, _, err := net.SplitHostPort(value); err != nil {
		return "", fmt.Errorf("invalid %s: %w", key, err)
	}
	return value, nil
}

func absolutePath(lookup func(string) (string, bool), key string, fallback string) (string, error) {
	value := valueOrDefault(lookup, key, fallback)
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", key, err)
	}
	return absolute, nil
}

func valueOrDefault(lookup func(string) (string, bool), key string, fallback string) string {
	if value, ok := lookup(key); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

// PublicAddress is the host and port peers dial. PublicHostname may carry a
// port, for a node behind a port forward that differs from its listener;
// otherwise the federation listener's port is used.
func (cfg Config) PublicAddress() string {
	if _, _, err := net.SplitHostPort(cfg.PublicHostname); err == nil {
		return cfg.PublicHostname
	}
	_, port, err := net.SplitHostPort(cfg.FederationListenAddress)
	if err != nil || cfg.PublicHostname == "" {
		return cfg.PublicHostname
	}
	return net.JoinHostPort(cfg.PublicHostname, port)
}

func isLoopback(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
