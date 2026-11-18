// Package api serves /health and the bearer-protected stats REST API that
// Projects Monitor reads. It listens on its own address so nothing on the
// proxy port can collide with a GitHub API path.
package api

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/guibranco/github-api-proxy/internal/store"
)

const maxPeriod = 90 * 24 * time.Hour

//go:embed openapi.yaml
var openAPISpec string

// Icons for browsers visiting the admin host (Swagger UI). Design sources live in docs/images.
var (
	//go:embed assets/favicon.ico
	faviconICO []byte
	//go:embed assets/favicon.svg
	faviconSVG []byte
	//go:embed assets/apple-touch-icon.png
	appleTouchIcon []byte
)

// swaggerUIVersion is the pinned swagger-ui-dist release served from jsDelivr.
// To upgrade: bump it and refresh the three integrity hashes below
// (openssl dgst -sha384 -binary FILE | openssl base64 -A on the npm tarball files).
const swaggerUIVersion = "5.35.0"

// swaggerPage renders Swagger UI against /openapi.yaml using the standalone
// layout, whose top bar carries Swagger UI's own light/dark switch. The first
// visit follows the OS theme; the switch's choice is remembered per browser.
const swaggerPage = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="light dark">
  <title>github-api-proxy — API</title>
  <link rel="icon" href="/favicon.ico" sizes="any">
  <link rel="icon" href="/favicon.svg" type="image/svg+xml">
  <link rel="apple-touch-icon" href="/apple-touch-icon.png">
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui.css"
        integrity="sha384-WlYgdmXYOOBWL5apwjDEZwN+U3HmXSSu5R6V5aGxEcf7z+KuRNwBhoxKVN1i3D5I" crossorigin="anonymous">
  <style>
    body { margin: 0; }
    html.dark-mode body { background: #1c1c21; }
    /* keep the top bar for the theme switch; the spec URL box is not useful here */
    .swagger-ui .topbar .download-url-wrapper { display: none; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui-bundle.js"
          integrity="sha384-gjRm0bEPMeTmMh5jT2cHccIwCUZSTd8YC+j0d0sXw0MdGEfScduGpSyF3ZSSLkX9" crossorigin="anonymous"></script>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@` + swaggerUIVersion + `/swagger-ui-standalone-preset.js"
          integrity="sha384-My2aDM4r2Mbm3ybHcubKm9O9U8FEjvF/O5nGvE9YK5dzqOTbWEKa79RPJ1krdMaF" crossorigin="anonymous"></script>
  <script>
    (function () {
      var KEY = "github-api-proxy.swagger.theme";
      var root = document.documentElement;
      function stored() { try { return localStorage.getItem(KEY); } catch (e) { return null; } }
      function store(v) { try { localStorage.setItem(KEY, v); } catch (e) { /* storage unavailable */ } }

      window.ui = SwaggerUIBundle({
        url: "openapi.yaml",
        dom_id: "#swagger-ui",
        deepLinking: true,
        persistAuthorization: true,
        displayRequestDuration: true,
        tryItOutEnabled: true,
        presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
        plugins: [SwaggerUIBundle.plugins.DownloadUrl],
        layout: "StandaloneLayout",
        onComplete: function () {
          // Re-apply the saved choice through Swagger UI's own switch so its icon stays in sync.
          var want = stored();
          var isDark = root.classList.contains("dark-mode");
          var toggle = document.querySelector(".dark-mode-toggle button");
          if (toggle && ((want === "dark" && !isDark) || (want === "light" && isDark))) {
            toggle.click();
          }
          // Remember every later use of the switch.
          new MutationObserver(function () {
            store(root.classList.contains("dark-mode") ? "dark" : "light");
          }).observe(root, { attributes: true, attributeFilter: ["class"] });
        }
      });
    })();
  </script>
</body>
</html>`

// AllowListStatus describes the network allow-list for /health.
type AllowListStatus interface {
	Status() (allowAll bool, networks int)
}

// API is the admin/stats HTTP handler.
type API struct {
	store     *store.Store
	token     string
	version   string
	started   time.Time
	log       *slog.Logger
	allowList AllowListStatus
	spec      []byte
	mux       *http.ServeMux
}

// New builds the handler.
func New(s *store.Store, token, version string, started time.Time, allowList AllowListStatus, log *slog.Logger) *API {
	a := &API{store: s, token: token, version: version, started: started, allowList: allowList, log: log,
		spec: []byte(strings.Replace(openAPISpec, "{{VERSION}}", version, 1)), mux: http.NewServeMux()}
	a.mux.HandleFunc("GET /health", a.health)
	a.mux.HandleFunc("GET /openapi.yaml", a.openAPI)
	a.mux.HandleFunc("GET /favicon.ico", staticAsset("image/x-icon", faviconICO))
	a.mux.HandleFunc("GET /favicon.svg", staticAsset("image/svg+xml", faviconSVG))
	a.mux.HandleFunc("GET /apple-touch-icon.png", staticAsset("image/png", appleTouchIcon))
	a.mux.HandleFunc("GET /swagger", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/", http.StatusMovedPermanently)
	})
	a.mux.HandleFunc("GET /swagger/{$}", a.swagger)
	a.mux.HandleFunc("GET /swagger/openapi.yaml", a.openAPI)
	a.mux.HandleFunc("GET /api/v1/summary", a.auth(a.summary))
	a.mux.HandleFunc("GET /api/v1/callers", a.auth(a.callers))
	a.mux.HandleFunc("GET /api/v1/routes", a.auth(a.routes))
	a.mux.HandleFunc("GET /api/v1/timeline", a.auth(a.timeline))
	a.mux.HandleFunc("GET /api/v1/requests", a.auth(a.requests))
	a.mux.HandleFunc("GET /api/v1/rate-limits", a.auth(a.rateLimits))
	return a
}

// ServeHTTP implements http.Handler.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	a.mux.ServeHTTP(w, r)
}

func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	expected := []byte("Bearer " + a.token)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.TrimSpace(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (a *API) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(a.spec)
}

// staticAsset serves an embedded file. Icons change only with a release, so unlike
// the API responses (no-store, set in ServeHTTP) they may be cached for a day.
func staticAsset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(body)
	}
}

func (a *API) swagger(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(swaggerPage))
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	dbStatus, status, code := "ok", "ok", http.StatusOK
	if err := a.store.Ping(ctx); err != nil {
		// The proxy keeps forwarding while MySQL is down; report degraded, not down.
		dbStatus, status = "error", "degraded"
	}
	writer := a.store.Stats()
	if writer.Failing {
		status = "degraded"
	}
	allowAll, networks := a.allowList.Status()
	writeJSON(w, code, map[string]any{
		"allow_list":     map[string]any{"allow_all": allowAll, "networks": networks},
		"status":         status,
		"version":        a.version,
		"started_at":     a.started.UTC(),
		"uptime_seconds": int64(time.Since(a.started).Seconds()),
		"database":       dbStatus,
		"writer":         writer,
	})
}

// period parses ?period=24h|7d|90m (default 24h, max 90d) into a start time.
func period(r *http.Request) (time.Time, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("period"))
	if raw == "" {
		raw = "24h"
	}
	var d time.Duration
	if strings.HasSuffix(raw, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err != nil {
			return time.Time{}, false
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(raw); err != nil {
			return time.Time{}, false
		}
	}
	if d <= 0 || d > maxPeriod {
		return time.Time{}, false
	}
	return time.Now().UTC().Add(-d), true
}

func intParam(r *http.Request, name string, def, lo, hi int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return max(lo, min(hi, n))
}

func (a *API) withPeriod(w http.ResponseWriter, r *http.Request, fn func(since time.Time) (any, error)) {
	since, ok := period(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid period: use e.g. 90m, 24h or 7d (max 90d)"})
		return
	}
	data, err := fn(since)
	if err != nil {
		a.log.Error("Stats query failed", "category", "api", "path", r.URL.Path, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"since": since, "data": data})
}

func (a *API) summary(w http.ResponseWriter, r *http.Request) {
	a.withPeriod(w, r, func(since time.Time) (any, error) { return a.store.Summary(r.Context(), since) })
}

func (a *API) callers(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100, 1, 500)
	a.withPeriod(w, r, func(since time.Time) (any, error) { return a.store.Callers(r.Context(), since, limit) })
}

func (a *API) routes(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 20, 1, 200)
	caller := r.URL.Query().Get("caller")
	a.withPeriod(w, r, func(since time.Time) (any, error) { return a.store.Routes(r.Context(), since, caller, limit) })
}

func (a *API) timeline(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		bucket = "hour"
	}
	if bucket != "minute" && bucket != "hour" && bucket != "day" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bucket must be minute, hour or day"})
		return
	}
	caller := r.URL.Query().Get("caller")
	a.withPeriod(w, r, func(since time.Time) (any, error) { return a.store.Timeline(r.Context(), since, bucket, caller) })
}

func (a *API) requests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before_id"), 10, 64)
	status := 0
	if s := q.Get("status"); s != "" {
		// "404" exact, "4xx" / "5xx" class
		if strings.HasSuffix(strings.ToLower(s), "xx") {
			status, _ = strconv.Atoi(s[:1])
		} else {
			status, _ = strconv.Atoi(s)
		}
	}
	f := store.RequestFilter{
		Caller: q.Get("caller"), Method: q.Get("method"), Route: q.Get("route"),
		Status: status, BeforeID: before, Limit: intParam(r, "limit", 50, 1, 500),
	}
	a.withPeriod(w, r, func(since time.Time) (any, error) {
		f.Since = since
		return a.store.Requests(r.Context(), f)
	})
}

func (a *API) rateLimits(w http.ResponseWriter, r *http.Request) {
	// Rate-limit windows are at most an hour long; look back a bit further.
	since := time.Now().UTC().Add(-2 * time.Hour)
	data, err := a.store.RateLimits(r.Context(), since)
	if err != nil {
		a.log.Error("Stats query failed", "category", "api", "path", r.URL.Path, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"since": since, "data": data})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
