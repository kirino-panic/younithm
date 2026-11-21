// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// Environment variable names.
const (
	EnvAddr               = "YOUNITHM_ADDR"
	EnvConfigDir          = "YOUNITHM_CONFIG_DIR"
	EnvTrustedProxies     = "YOUNITHM_TRUSTED_PROXIES"
	EnvTrustedProxyAll    = "YOUNITHM_TRUSTED_PROXY"
	EnvMusicBrainzContact = "MUSICBRAINZ_CONTACT"
	EnvEnableITunes       = "YOUNITHM_ENABLE_ITUNES"
	EnvDebugUI            = "YOUNITHM_DEBUG_UI"
)

type Config struct {
	// Addr is the listen address, e.g. ":2015".
	Addr string
	// ConfigDir holds the SQLite DB, key file, artwork cache and backups.
	// In Docker this is the /config volume.
	ConfigDir string
	// TrustedProxies is a comma-separated list of CIDRs / IPs of reverse
	// proxies whose X-Forwarded-* headers are honored (e.g. "172.18.0.0/16").
	// Use ProxyPrefixes to parse it. Empty means no proxy is trusted.
	TrustedProxies string
	// TrustAllProxies is the blunt flag (YOUNITHM_TRUSTED_PROXY=true): honor
	// X-Forwarded-* from any peer. Only safe when the app port is reachable
	// solely through the proxy. Prefer TrustedProxies.
	TrustAllProxies bool
	// MusicBrainzContact is the email or URL put in the MusicBrainz
	// User-Agent (MUSICBRAINZ_CONTACT). MusicBrainz asks for one.
	MusicBrainzContact string
	// EnableITunes turns on the keyless iTunes Search fallback, which sends
	// artist and title text to Apple while no Apple Music keys are configured.
	// Off by default: nothing is sent to a provider the user has not set up.
	EnableITunes bool
	// DebugUI mounts the unauthenticated, loopback-only developer UI at
	// /debug/ (YOUNITHM_DEBUG_UI=1). Development only.
	DebugUI bool
}

func Load() Config {
	all, _ := strconv.ParseBool(os.Getenv(EnvTrustedProxyAll))
	iTunes, _ := strconv.ParseBool(os.Getenv(EnvEnableITunes))
	debugUI, _ := strconv.ParseBool(os.Getenv(EnvDebugUI))
	return Config{
		Addr:               env(EnvAddr, ":2015"),
		ConfigDir:          env(EnvConfigDir, "/config"),
		TrustedProxies:     strings.TrimSpace(os.Getenv(EnvTrustedProxies)),
		TrustAllProxies:    all,
		MusicBrainzContact: strings.TrimSpace(os.Getenv(EnvMusicBrainzContact)),
		EnableITunes:       iTunes,
		DebugUI:            debugUI,
	}
}

// ProxyPrefixes parses TrustedProxies. Bare IPs become /32 or /128.
func (c Config) ProxyPrefixes() ([]netip.Prefix, error) {
	return ParseProxies(c.TrustedProxies)
}

// ParseProxies parses a comma-separated CIDR/IP list.
func ParseProxies(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(part)
		if err != nil {
			return nil, fmt.Errorf("%s: %q is not an IP or CIDR", EnvTrustedProxies, part)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// Validate reports configuration mistakes worth failing startup for.
func (c Config) Validate() error {
	if _, err := c.ProxyPrefixes(); err != nil {
		return err
	}
	_, err := ParseBasePath(os.Getenv(EnvBasePath))
	return err
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
