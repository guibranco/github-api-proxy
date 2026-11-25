// Package config builds the proxy settings. In production everything comes
// from Grimoire; the host only knows the three Grimoire bootstrap values:
//
//	Grimoire__BaseUrl, Grimoire__ApiKey, Grimoire__Environment (default "production")
//
// Resolution order, last wins: built-in defaults → Grimoire configurations →
// Grimoire secrets → environment variables (the key with ":" written as "__",
// e.g. Proxy__ListenAddr). If Grimoire is configured but unreachable, rejects
// the key or does not know the environment, Load fails so the service refuses
// to start instead of running on partial settings.
package config

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/guibranco/github-api-proxy/internal/grimoire"
)

// Configuration keys (Grimoire configurations, colon-separated).
const (
	KeyListenAddr           = "Proxy:ListenAddr"
	KeyAdminListenAddr      = "Proxy:AdminListenAddr"
	KeyUpstreamURL          = "Proxy:UpstreamUrl"
	KeyPublicBaseURL        = "Proxy:PublicBaseUrl"
	KeyTrustedProxies       = "Proxy:TrustedProxies"
	KeyUpstreamTimeout      = "Proxy:UpstreamTimeout"
	KeySlowRequest          = "Proxy:SlowRequestThreshold"
	KeyRateLimitWarnPercent = "Proxy:RateLimitWarnPercent"
	KeyResolveIdentities    = "Proxy:ResolveIdentities"

	KeyAllowedSources  = "Network:AllowedSources"
	KeyRefreshInterval = "Network:RefreshInterval"
	KeyPublicHealth    = "Network:PublicHealth"

	// KeyGrimoireRefreshInterval is how often Grimoire's sentinel keys are
	// checked for published changes (a 304 while nothing changed).
	KeyGrimoireRefreshInterval = "Grimoire:RefreshInterval"

	KeyRetentionDays = "Storage:RetentionDays"
	KeyQueueSize     = "Storage:QueueSize"
	KeyBatchSize     = "Storage:BatchSize"
	KeyFlushInterval = "Storage:FlushInterval"

	KeyLogStreamEnabled   = "LogStream:Enabled"
	KeyLogStreamServer    = "LogStream:Server"
	KeyLogStreamAPIKey    = "LogStream:ApiKey"
	KeyLogStreamAppID     = "LogStream:AppId"
	KeyLogStreamMinLevel  = "LogStream:MinimumLevel"
	KeyLogStreamTransport = "LogStream:Transport"
	KeyLogStreamUDPHost   = "LogStream:UdpHost"
	KeyLogStreamUDPPort   = "LogStream:UdpPort"
	KeyLogStreamUDPMax    = "LogStream:UdpMaxPacketSize"

	// Secrets (read as Grimoire secrets named with "--" for ":", Azure Key Vault style).
	KeyMySQLDSN          = "MySql:ConnectionString"
	KeyAdminAPIToken     = "Admin:ApiToken"
	KeyLogStreamAPIToken = "LogStream:ApiToken"
)

// SecretKeys are fetched from Grimoire by name (Grimoire cannot list secrets).
var SecretKeys = []string{KeyMySQLDSN, KeyAdminAPIToken, KeyLogStreamAPIToken}

var allKeys = []string{
	KeyListenAddr, KeyAdminListenAddr, KeyUpstreamURL, KeyPublicBaseURL, KeyTrustedProxies,
	KeyUpstreamTimeout, KeySlowRequest, KeyRateLimitWarnPercent, KeyResolveIdentities,
	KeyAllowedSources, KeyRefreshInterval, KeyPublicHealth, KeyGrimoireRefreshInterval,
	KeyRetentionDays, KeyQueueSize, KeyBatchSize, KeyFlushInterval,
	KeyLogStreamEnabled, KeyLogStreamServer, KeyLogStreamAPIKey, KeyLogStreamAppID, KeyLogStreamMinLevel,
	KeyLogStreamTransport, KeyLogStreamUDPHost, KeyLogStreamUDPPort, KeyLogStreamUDPMax,
	KeyMySQLDSN, KeyAdminAPIToken, KeyLogStreamAPIToken,
}

// Config holds every runtime setting.
type Config struct {
	ListenAddr           string
	AdminListenAddr      string
	UpstreamURL          *url.URL
	PublicBaseURL        *url.URL
	TrustedProxies       []*net.IPNet
	UpstreamTimeout      time.Duration
	SlowRequest          time.Duration
	RateLimitWarnPercent int
	ResolveIdentity      bool

	// AllowedSources is the raw allow-list (IPs, CIDRs, host names; "*" disables it).
	AllowedSources string
	// RefreshInterval is how often allow-list host names are re-resolved.
	RefreshInterval time.Duration
	PublicHealth    bool
	// GrimoireRefreshInterval is how often Grimoire's sentinels are checked.
	GrimoireRefreshInterval time.Duration

	MySQLDSN         string
	AdminAPIToken    string
	RetentionDays    int
	LogQueueSize     int
	LogBatchSize     int
	LogFlushInterval time.Duration

	LogStream LogStreamConfig

	// Origin describes where settings came from, for the startup log.
	Origin string
}

// LogStreamConfig configures delivery of the proxy's own events to LogStream
// (MariaDB storage mode: the API key is the app key, the token signs UDP packets).
type LogStreamConfig struct {
	Enabled       bool
	ServerURL     string
	APIKey        string
	APIToken      string
	AppID         string
	MinLevel      string
	Transport     string // "udp" or "http"
	UDPHost       string
	UDPPort       int
	UDPMaxPacket  int
	DisableReason string
}

// Ready reports whether LogStream delivery can run.
func (l LogStreamConfig) Ready() bool {
	return l.Enabled && l.ServerURL != "" && l.APIKey != "" && l.APIToken != ""
}

// Bootstrap is how to reach Grimoire.
type Bootstrap struct {
	BaseURL     string
	APIKey      string
	Environment string
}

// BootstrapFromEnv reads Grimoire__BaseUrl / Grimoire__ApiKey / Grimoire__Environment
// (GRIMOIRE_BASE_URL / GRIMOIRE_API_KEY / GRIMOIRE_ENVIRONMENT also accepted).
func BootstrapFromEnv() Bootstrap {
	pick := func(names ...string) string {
		for _, n := range names {
			if v := strings.TrimSpace(os.Getenv(n)); v != "" {
				return v
			}
		}
		return ""
	}
	b := Bootstrap{
		BaseURL:     pick("Grimoire__BaseUrl", "GRIMOIRE_BASE_URL"),
		APIKey:      pick("Grimoire__ApiKey", "GRIMOIRE_API_KEY"),
		Environment: pick("Grimoire__Environment", "GRIMOIRE_ENVIRONMENT"),
	}
	if b.Environment == "" {
		b.Environment = "production"
	}
	return b
}

// Load builds the configuration. It returns the Grimoire client (nil when
// Grimoire is not configured) so callers can refresh live settings.
func Load(ctx context.Context, b Bootstrap, version, userAgent string) (*Config, *grimoire.Client, error) {
	values := map[string]string{}
	origin := "environment only (Grimoire not configured)"
	var client *grimoire.Client

	if b.BaseURL != "" || b.APIKey != "" {
		if b.BaseURL == "" || b.APIKey == "" {
			return nil, nil, errors.New("both Grimoire__BaseUrl and Grimoire__ApiKey must be set")
		}
		client = grimoire.New(b.BaseURL, b.APIKey, b.Environment, userAgent)
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		configs, err := client.Configurations(cctx)
		if err != nil {
			return nil, nil, fmt.Errorf("loading configurations from Grimoire: %w", err)
		}
		for k, v := range configs {
			values[strings.ToLower(k)] = v
		}
		fetched := 0
		for _, key := range SecretKeys {
			v, err := client.Secret(cctx, strings.ReplaceAll(key, ":", "--"))
			if errors.Is(err, grimoire.ErrNotFound) {
				continue // the validation below reports required ones
			}
			if err != nil {
				return nil, nil, fmt.Errorf("loading secret %s from Grimoire: %w", key, err)
			}
			values[strings.ToLower(key)] = v
			fetched++
		}
		origin = fmt.Sprintf("Grimoire environment %q (%d configurations, %d secrets)", b.Environment, len(configs), fetched)
	}

	overridden := 0
	for _, key := range allKeys {
		if v, ok := lookupEnv(key); ok {
			values[strings.ToLower(key)] = v
			overridden++
		}
	}
	if overridden > 0 {
		origin += fmt.Sprintf(", %d overridden by environment", overridden)
	}

	cfg, err := build(settings(values), version)
	if cfg != nil {
		cfg.Origin = origin
	}
	return cfg, client, err
}

func lookupEnv(key string) (string, bool) {
	name := strings.ReplaceAll(key, ":", "__")
	for _, n := range []string{name, strings.ToUpper(name)} {
		if v, ok := os.LookupEnv(n); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

type settings map[string]string

func (s settings) str(key, def string) string {
	if v := strings.TrimSpace(s[strings.ToLower(key)]); v != "" {
		return v
	}
	return def
}

func build(s settings, version string) (*Config, error) {
	var errs []error
	intv := func(key string, def int) int {
		v := s.str(key, "")
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return def
		}
		return n
	}
	boolv := func(key string, def bool) bool {
		v := s.str(key, "")
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return def
		}
		return b
	}
	durv := func(key string, def time.Duration) time.Duration {
		v := s.str(key, "")
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return def
		}
		return d
	}

	upstream, err := parseURL(s.str(KeyUpstreamURL, "https://api.github.com"))
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", KeyUpstreamURL, err))
	}
	var public *url.URL
	if v := s.str(KeyPublicBaseURL, ""); v != "" {
		if public, err = parseURL(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", KeyPublicBaseURL, err))
		}
	}
	trusted, err := ParseCIDRs(s.str(KeyTrustedProxies, "127.0.0.1,::1"))
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", KeyTrustedProxies, err))
	}

	cfg := &Config{
		ListenAddr:           s.str(KeyListenAddr, "127.0.0.1:8080"),
		AdminListenAddr:      s.str(KeyAdminListenAddr, "127.0.0.1:8081"),
		UpstreamURL:          upstream,
		PublicBaseURL:        public,
		TrustedProxies:       trusted,
		UpstreamTimeout:      durv(KeyUpstreamTimeout, 60*time.Second),
		SlowRequest:          durv(KeySlowRequest, 10*time.Second),
		RateLimitWarnPercent: intv(KeyRateLimitWarnPercent, 10),
		ResolveIdentity:      boolv(KeyResolveIdentities, true),

		AllowedSources:  s.str(KeyAllowedSources, ""),
		RefreshInterval: durv(KeyRefreshInterval, 5*time.Minute),
		PublicHealth:    boolv(KeyPublicHealth, false),

		GrimoireRefreshInterval: durv(KeyGrimoireRefreshInterval, 30*time.Second),

		MySQLDSN:         s.str(KeyMySQLDSN, ""),
		AdminAPIToken:    s.str(KeyAdminAPIToken, ""),
		RetentionDays:    intv(KeyRetentionDays, 30),
		LogQueueSize:     intv(KeyQueueSize, 10000),
		LogBatchSize:     intv(KeyBatchSize, 200),
		LogFlushInterval: durv(KeyFlushInterval, time.Second),
	}

	serverURL := strings.TrimRight(s.str(KeyLogStreamServer, ""), "/")
	udpHost := s.str(KeyLogStreamUDPHost, "")
	if udpHost == "" && serverURL != "" {
		if u, err := url.Parse(serverURL); err == nil {
			udpHost = u.Hostname() // the UDP listener runs on the LogStream server's host
		}
	}
	cfg.LogStream = LogStreamConfig{
		Enabled:      boolv(KeyLogStreamEnabled, true),
		ServerURL:    serverURL,
		APIKey:       s.str(KeyLogStreamAPIKey, ""),
		APIToken:     s.str(KeyLogStreamAPIToken, ""),
		AppID:        s.str(KeyLogStreamAppID, version), // default: the GitVersion semVer of this build
		MinLevel:     s.str(KeyLogStreamMinLevel, "info"),
		Transport:    strings.ToLower(s.str(KeyLogStreamTransport, "udp")),
		UDPHost:      udpHost,
		UDPPort:      intv(KeyLogStreamUDPPort, 8082),
		UDPMaxPacket: intv(KeyLogStreamUDPMax, 8000),
	}
	switch {
	case !cfg.LogStream.Enabled:
		cfg.LogStream.DisableReason = KeyLogStreamEnabled + " is false"
	case serverURL == "":
		cfg.LogStream.DisableReason = KeyLogStreamServer + " is not set"
	case cfg.LogStream.APIKey == "" || cfg.LogStream.APIToken == "":
		cfg.LogStream.DisableReason = KeyLogStreamAPIKey + " or the LogStream--ApiToken secret is missing"
	}
	if cfg.LogStream.Transport != "udp" && cfg.LogStream.Transport != "http" {
		errs = append(errs, fmt.Errorf("%s must be Udp or Http", KeyLogStreamTransport))
	}

	if cfg.MySQLDSN == "" {
		errs = append(errs, fmt.Errorf("%s is required (Grimoire secret MySql--ConnectionString)", KeyMySQLDSN))
	}
	if len(cfg.AdminAPIToken) < 24 {
		errs = append(errs, fmt.Errorf("%s is required and must be at least 24 characters (Grimoire secret Admin--ApiToken)", KeyAdminAPIToken))
	}
	if cfg.AllowedSources == "" {
		errs = append(errs, fmt.Errorf("%s is required: list the IPs, CIDRs or host names allowed to call the proxy (\"*\" allows everyone)", KeyAllowedSources))
	}
	if cfg.RefreshInterval < 30*time.Second {
		errs = append(errs, fmt.Errorf("%s must be at least 30s", KeyRefreshInterval))
	}
	if cfg.GrimoireRefreshInterval < 5*time.Second {
		errs = append(errs, fmt.Errorf("%s must be at least 5s", KeyGrimoireRefreshInterval))
	}
	if cfg.RetentionDays < 1 {
		errs = append(errs, fmt.Errorf("%s must be >= 1", KeyRetentionDays))
	}
	if cfg.LogQueueSize < 100 || cfg.LogBatchSize < 1 || cfg.LogBatchSize > 1000 {
		errs = append(errs, fmt.Errorf("%s must be >= 100 and %s between 1 and 1000", KeyQueueSize, KeyBatchSize))
	}
	return cfg, errors.Join(errs...)
}

func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%q must be an absolute http(s) URL", raw)
	}
	return u, nil
}

// ParseCIDRs parses a comma-separated list of IPs and CIDR ranges.
func ParseCIDRs(raw string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\n' }) {
		n, err := ParseCIDR(part)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// ParseCIDR parses one IP (as a /32 or /128) or CIDR range.
func ParseCIDR(part string) (*net.IPNet, error) {
	part = strings.TrimSpace(part)
	if !strings.Contains(part, "/") {
		ip := net.ParseIP(part)
		if ip == nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR range", part)
		}
		if ip.To4() != nil {
			return &net.IPNet{IP: ip.To4(), Mask: net.CIDRMask(32, 32)}, nil
		}
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)}, nil
	}
	_, n, err := net.ParseCIDR(part)
	return n, err
}
