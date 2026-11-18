// Package proxy is the transparent GitHub API reverse proxy. Clients keep
// their own credentials and only change the base URL; every request is
// forwarded to api.github.com as-is and recorded asynchronously.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/guibranco/github-api-proxy/internal/config"
	"github.com/guibranco/github-api-proxy/internal/identity"
	"github.com/guibranco/github-api-proxy/internal/store"
)

const maxGraphQLPeek = 1 << 20 // read at most 1 MiB of a GraphQL body to label it

// Recorder receives finished request records (the MySQL store).
type Recorder interface {
	Enqueue(store.Record)
}

// Observer labels callers (the identity resolver).
type Observer interface {
	Observe(identity.Credential) string
}

type ctxKey struct{}

type requestInfo struct {
	prefix     string
	publicBase string
	rec        *store.Record
	upstreamOK bool
}

// Handler is the proxy http.Handler.
type Handler struct {
	cfg      *config.Config
	rp       *httputil.ReverseProxy
	recorder Recorder
	observer Observer
	log      *slog.Logger
	upstream string // e.g. https://api.github.com
	throttle *throttle
}

// New builds the proxy handler.
func New(cfg *config.Config, recorder Recorder, observer Observer, log *slog.Logger) *Handler {
	h := &Handler{
		cfg:      cfg,
		recorder: recorder,
		observer: observer,
		log:      log,
		upstream: cfg.UpstreamURL.Scheme + "://" + cfg.UpstreamURL.Host,
		throttle: newThrottle(),
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 200
	transport.MaxIdleConnsPerHost = 100
	transport.IdleConnTimeout = 90 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = cfg.UpstreamTimeout
	transport.ForceAttemptHTTP2 = true
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext

	h.rp = &httputil.ReverseProxy{
		Transport:      transport,
		Rewrite:        h.rewrite,
		ModifyResponse: h.modifyResponse,
		ErrorHandler:   h.errorHandler,
		ErrorLog:       slog.NewLogLogger(log.With("category", "http").Handler(), slog.LevelWarn),
	}
	return h
}

func (h *Handler) rewrite(pr *httputil.ProxyRequest) {
	mapped, _ := upstreamPath(pr.In.URL.EscapedPath())

	pr.Out.URL.Scheme = h.cfg.UpstreamURL.Scheme
	pr.Out.URL.Host = h.cfg.UpstreamURL.Host
	pr.Out.URL.RawPath = mapped
	if p, err := url.PathUnescape(mapped); err == nil {
		pr.Out.URL.Path = p
	} else {
		pr.Out.URL.Path = mapped
	}
	if pr.Out.URL.RawPath == pr.Out.URL.Path {
		pr.Out.URL.RawPath = ""
	}
	pr.Out.Host = h.cfg.UpstreamURL.Host
	// Rewrite already strips Forwarded / X-Forwarded-*; GitHub does not need to
	// see the proxy chain either.
	pr.Out.Header.Del("X-Real-IP")
}

func (h *Handler) modifyResponse(resp *http.Response) error {
	info := resp.Request.Context().Value(ctxKey{}).(*requestInfo)
	info.upstreamOK = true

	for _, name := range []string{"Link", "Location", "Content-Location"} {
		if v := resp.Header.Get(name); v != "" {
			resp.Header.Set(name, rewriteAbsolute(v, h.upstream, info.publicBase, info.prefix))
		}
	}

	rec := info.rec
	rec.RateLimitResource = resp.Header.Get("X-RateLimit-Resource")
	rec.RateLimitLimit = headerInt(resp.Header, "X-RateLimit-Limit")
	rec.RateLimitRemaining = headerInt(resp.Header, "X-RateLimit-Remaining")
	rec.RateLimitUsed = headerInt(resp.Header, "X-RateLimit-Used")
	if reset := headerInt(resp.Header, "X-RateLimit-Reset"); reset > 0 {
		rec.RateLimitReset = time.Unix(int64(reset), 0).UTC()
	}
	rec.GitHubRequestID = resp.Header.Get("X-GitHub-Request-Id")
	return nil
}

func (h *Handler) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	info := r.Context().Value(ctxKey{}).(*requestInfo)
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(r.Context().Err(), context.Canceled):
		info.rec.Status = 499 // client closed the connection
		info.rec.Error = "client closed request"
		return
	case isTimeout(err):
		status = http.StatusGatewayTimeout
	}
	info.rec.Error = err.Error()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":"github-api-proxy: upstream request to GitHub failed (%s)","documentation_url":"https://docs.github.com/rest"}`,
		http.StatusText(status))
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded)
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	cred := identity.FromRequest(r)
	caller := h.observer.Observe(cred)
	mappedPath, prefix := upstreamPath(r.URL.Path)

	rec := &store.Record{
		RequestedAt:        start.UTC(),
		Method:             r.Method,
		Path:               mappedPath,
		Route:              Route(mappedPath),
		Query:              sanitizeQuery(r.URL.RawQuery),
		ClientIP:           ClientIP(r, h.cfg.TrustedProxies),
		UserAgent:          r.UserAgent(),
		TokenHash:          cred.Hash,
		TokenKind:          cred.Kind,
		RateLimitLimit:     -1,
		RateLimitRemaining: -1,
		RateLimitUsed:      -1,
	}

	// Label GraphQL calls: every one of them is POST /graphql, so the path alone says nothing.
	if r.Method == http.MethodPost && mappedPath == "/graphql" && r.Body != nil {
		peek, err := io.ReadAll(io.LimitReader(r.Body, maxGraphQLPeek+1))
		if err == nil && len(peek) <= maxGraphQLPeek {
			rec.GraphQLOperation = graphQLOperation(peek)
		}
		r.Body = readCloser{io.MultiReader(bytes.NewReader(peek), r.Body), r.Body}
	}
	body := &countingBody{rc: r.Body}
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = body
	}

	info := &requestInfo{prefix: prefix, publicBase: publicBase(r, h.cfg.PublicBaseURL, h.cfg.TrustedProxies), rec: rec}
	rw := &responseWriter{ResponseWriter: w}
	h.rp.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), ctxKey{}, info)))

	rec.Duration = time.Since(start)
	if rec.Status == 0 {
		rec.Status = rw.status
		if rec.Status == 0 {
			rec.Status = http.StatusOK
		}
	}
	rec.RequestBytes = body.n
	rec.ResponseBytes = rw.bytes
	h.recorder.Enqueue(*rec)
	h.report(rec, caller, info.upstreamOK)
}

// report sends noteworthy events to the logger (and so to LogStream), throttled.
func (h *Handler) report(rec *store.Record, caller string, upstreamOK bool) {
	fields := func(extra ...any) []any {
		base := []any{"caller", caller, "method", rec.Method, "path", rec.Path, "route", rec.Route,
			"status", rec.Status, "duration_ms", rec.Duration.Milliseconds(), "client_ip", rec.ClientIP,
			"user_agent", rec.UserAgent, "github_request_id", rec.GitHubRequestID}
		return append(base, extra...)
	}

	switch {
	case rec.Status == 499:
		return
	case !upstreamOK:
		if ok, n := h.throttle.allow("upstream-failure", 30*time.Second); ok {
			h.log.Error("Upstream request to GitHub failed", fields("category", "upstream", "error", rec.Error, "suppressed", n)...)
		}
		return
	case rec.Status >= 500:
		if ok, n := h.throttle.allow("5xx:"+rec.Route, time.Minute); ok {
			h.log.Warn("GitHub answered with a server error", fields("category", "upstream", "suppressed", n)...)
		}
	}

	if (rec.Status == http.StatusForbidden || rec.Status == http.StatusTooManyRequests) && rec.RateLimitRemaining == 0 {
		key := fmt.Sprintf("limited:%s:%s:%d", rec.TokenHash+rec.ClientIP, rec.RateLimitResource, rec.RateLimitReset.Unix())
		if ok, _ := h.throttle.allow(key, time.Hour); ok {
			h.log.Warn("Caller is rate limited by GitHub", fields("category", "rate-limit",
				"resource", rec.RateLimitResource, "limit", rec.RateLimitLimit, "reset_at", rec.RateLimitReset)...)
		}
	} else if p := h.cfg.RateLimitWarnPercent; p > 0 && rec.RateLimitLimit > 0 && rec.RateLimitRemaining >= 0 &&
		rec.RateLimitRemaining*100 < rec.RateLimitLimit*p {
		key := fmt.Sprintf("low:%s:%s:%d", rec.TokenHash+rec.ClientIP, rec.RateLimitResource, rec.RateLimitReset.Unix())
		if ok, _ := h.throttle.allow(key, time.Hour); ok {
			h.log.Warn("Caller's GitHub rate limit is running low", fields("category", "rate-limit",
				"resource", rec.RateLimitResource, "limit", rec.RateLimitLimit, "remaining", rec.RateLimitRemaining,
				"reset_at", rec.RateLimitReset)...)
		}
	}

	if h.cfg.SlowRequest > 0 && rec.Duration > h.cfg.SlowRequest {
		if ok, n := h.throttle.allow("slow:"+rec.Route, time.Minute); ok {
			h.log.Warn("Slow GitHub API request", fields("category", "latency", "suppressed", n)...)
		}
	}
}

func headerInt(h http.Header, name string) int {
	v := h.Get(name)
	if v == "" {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return -1
	}
	return n
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type readCloser struct {
	io.Reader
	io.Closer
}

type countingBody struct {
	rc io.ReadCloser
	n  int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingBody) Close() error { return c.rc.Close() }

type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush on the real writer.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// throttle limits how often the same event is reported.
type throttle struct {
	mu         sync.Mutex
	last       map[string]time.Time
	suppressed map[string]int
}

func newThrottle() *throttle {
	return &throttle{last: map[string]time.Time{}, suppressed: map[string]int{}}
}

// allow returns true at most once per interval for a key, plus how many
// events were suppressed since the last allowed one.
func (t *throttle) allow(key string, every time.Duration) (bool, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if len(t.last) > 10000 {
		t.last, t.suppressed = map[string]time.Time{}, map[string]int{}
	}
	if last, ok := t.last[key]; ok && now.Sub(last) < every {
		t.suppressed[key]++
		return false, 0
	}
	n := t.suppressed[key]
	delete(t.suppressed, key)
	t.last[key] = now
	return true, n
}
