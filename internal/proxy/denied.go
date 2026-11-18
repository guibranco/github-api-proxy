package proxy

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/guibranco/github-api-proxy/internal/identity"
	"github.com/guibranco/github-api-proxy/internal/store"
)

// DeniedRecorder reports requests refused by the network allow-list: each one
// is stored (status 403, error "blocked by network allow-list", no identity
// lookup) and a warning goes to LogStream at most once per address every 10 minutes.
type DeniedRecorder struct {
	recorder Recorder
	log      *slog.Logger
	throttle *throttle
}

// NewDeniedRecorder creates the recorder.
func NewDeniedRecorder(recorder Recorder, log *slog.Logger) *DeniedRecorder {
	return &DeniedRecorder{recorder: recorder, log: log, throttle: newThrottle()}
}

// Record stores a blocked proxy request and logs it.
func (d *DeniedRecorder) Record(r *http.Request, ip string) {
	cred := identity.FromRequest(r)
	path, _ := upstreamPath(r.URL.Path)
	d.recorder.Enqueue(store.Record{
		RequestedAt: time.Now().UTC(), Method: r.Method, Path: path, Route: Route(path),
		Query: sanitizeQuery(r.URL.RawQuery), Status: http.StatusForbidden, ClientIP: ip,
		UserAgent: r.UserAgent(), TokenHash: cred.Hash, TokenKind: cred.Kind,
		RateLimitLimit: -1, RateLimitRemaining: -1, RateLimitUsed: -1, Error: store.BlockedError,
	})
	d.Log(r, ip)
}

// Log only reports a blocked request (used for the admin API).
func (d *DeniedRecorder) Log(r *http.Request, ip string) {
	if ok, n := d.throttle.allow("denied:"+ip, 10*time.Minute); ok {
		d.log.Warn("Request from an address outside the network allow-list was refused", "category", "network",
			"client_ip", ip, "method", r.Method, "path", r.URL.Path, "host", r.Host, "user_agent", r.UserAgent(), "suppressed", n)
	}
}
