// Package network restricts who may call the proxy: only the sources listed
// in Network:AllowedSources (IPs, CIDR ranges and host names such as
// node1.example.com or a dynamic-DNS name for a home connection). The list is
// re-read from Grimoire and host names are re-resolved on an interval, so
// adding a machine or following a changed home IP needs no restart.
package network

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/guibranco/github-api-proxy/internal/config"
)

// Source returns the current raw allow-list (e.g. read from Grimoire).
type Source func(ctx context.Context) (string, error)

// Resolver looks up host names (net.DefaultResolver in production).
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// loopback is always allowed: health checks and services on the same host.
var loopback = []*net.IPNet{
	{IP: net.IPv4(127, 0, 0, 0).To4(), Mask: net.CIDRMask(8, 32)},
	{IP: net.IPv6loopback, Mask: net.CIDRMask(128, 128)},
}

// AllowList is a periodically refreshed set of allowed networks.
type AllowList struct {
	source   Source
	resolver Resolver
	log      *slog.Logger

	mu       sync.RWMutex
	allowAll bool
	nets     []*net.IPNet
	raw      string
	hosts    map[string][]*net.IPNet // last good resolution per host name
	summary  string
}

// New creates an allow-list. Call Refresh once before serving.
func New(source Source, resolver Resolver, log *slog.Logger) *AllowList {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &AllowList{source: source, resolver: resolver, log: log, hosts: map[string][]*net.IPNet{}}
}

// Refresh re-reads the source and re-resolves host names. On a source error
// the previous list stays in force. It returns an error only when no list has
// ever been loaded.
func (a *AllowList) Refresh(ctx context.Context) error {
	raw, err := a.source(ctx)
	if err != nil {
		a.mu.RLock()
		loaded := a.raw != ""
		a.mu.RUnlock()
		if loaded {
			a.log.Warn("Could not refresh the network allow-list, keeping the previous one",
				"category", "network", "error", err)
			return nil
		}
		return err
	}

	allowAll := false
	var nets []*net.IPNet
	var problems []string
	hosts := map[string][]*net.IPNet{}

	for _, entry := range split(raw) {
		if entry == "*" {
			allowAll = true
			continue
		}
		if n, err := config.ParseCIDR(entry); err == nil {
			nets = append(nets, n)
			continue
		}
		host := strings.ToLower(entry)
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, err := a.resolver.LookupIPAddr(rctx, host)
		cancel()
		if err != nil || len(addrs) == 0 {
			a.mu.RLock()
			prev := a.hosts[host]
			a.mu.RUnlock()
			problems = append(problems, host)
			if len(prev) > 0 {
				hosts[host] = prev // keep the last good addresses during a DNS hiccup
				nets = append(nets, prev...)
			}
			continue
		}
		for _, ad := range addrs {
			n, _ := config.ParseCIDR(ad.IP.String())
			hosts[host] = append(hosts[host], n)
			nets = append(nets, n)
		}
	}
	if raw != "" && !allowAll && len(nets) == 0 && len(problems) == 0 {
		return fmt.Errorf("%s has no usable entries", config.KeyAllowedSources)
	}

	summary := describe(allowAll, nets)
	a.mu.Lock()
	changed := summary != a.summary
	a.allowAll, a.nets, a.raw, a.hosts, a.summary = allowAll, nets, raw, hosts, summary
	a.mu.Unlock()

	if len(problems) > 0 {
		a.log.Warn("Some allow-list host names did not resolve", "category", "network", "hosts", strings.Join(problems, ","))
	}
	if changed {
		a.log.Info("Network allow-list loaded", "category", "network", "allow_all", allowAll,
			"entries", len(nets), "networks", summary)
	}
	return nil
}

// Run refreshes the list every interval until ctx is done.
func (a *AllowList) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = a.Refresh(ctx)
		}
	}
}

// Status reports whether the list is open to everyone and how many networks it holds.
func (a *AllowList) Status() (allowAll bool, networks int) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.allowAll, len(a.nets)
}

// Allowed reports whether an address may call the proxy.
func (a *AllowList) Allowed(ip string) bool {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false
	}
	for _, n := range loopback {
		if n.Contains(parsed) {
			return true
		}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.allowAll {
		return true
	}
	for _, n := range a.nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// Middleware rejects requests from addresses outside the allow-list with a
// GitHub-style 403. clientIP extracts the real caller (honouring X-Forwarded-For
// only from trusted proxies); exempt paths (e.g. /health) skip the check.
func (a *AllowList) Middleware(next http.Handler, clientIP func(*http.Request) string,
	exempt func(*http.Request) bool, onDenied func(*http.Request, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exempt != nil && exempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		if !a.Allowed(ip) {
			if onDenied != nil {
				onDenied(r, ip)
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"github-api-proxy: requests from this address are not allowed","documentation_url":"https://github.com/guibranco/github-api-proxy#network-allow-list"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func split(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i] // "# comment" until the end of the line
		}
		for _, p := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\r'
		}) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func describe(allowAll bool, nets []*net.IPNet) string {
	if allowAll {
		return "*"
	}
	s := make([]string, 0, len(nets))
	for _, n := range nets {
		s = append(s, n.String())
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}
