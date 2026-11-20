package proxy

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// upstreamPath maps an incoming path to the GitHub API path. Clients that
// treat a custom base URL as GitHub Enterprise Server (Octokit.NET, gh-style
// tooling) send /api/v3/... and /api/graphql; both layouts are accepted.
// It also returns the prefix the client used, so absolute URLs we hand back
// keep that layout.
func upstreamPath(p string) (path, prefix string) {
	switch {
	case p == "/api/graphql":
		return "/graphql", "/api"
	case p == "/api/v3" || p == "/api/v3/":
		return "/", "/api/v3"
	case strings.HasPrefix(p, "/api/v3/"):
		return strings.TrimPrefix(p, "/api/v3"), "/api/v3"
	case p == "":
		return "/", ""
	default:
		return p, ""
	}
}

// rewriteAbsolute replaces the upstream origin in a header value (Link,
// Location) with the proxy's public base, so pagination and redirects stay on
// the proxy. For GraphQL ("/api") the REST links still need "/api/v3".
func rewriteAbsolute(value, upstreamBase, publicBase, prefix string) string {
	if value == "" || !strings.Contains(value, upstreamBase) {
		return value
	}
	if prefix == "/api" {
		prefix = "/api/v3"
	}
	return strings.ReplaceAll(value, upstreamBase, publicBase+prefix)
}

var (
	reNumeric = regexp.MustCompile(`^\d+$`)
	reSHA     = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)
)

// Route normalises a path into an endpoint template so the dashboard can group
// "/repos/guibranco/x/pulls/12" and "/repos/guibranco/y/pulls/7" as
// "/repos/{owner}/{repo}/pulls/{id}". The raw path is kept separately.
func Route(path string) string {
	if path == "" || path == "/" {
		return "/"
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	out := make([]string, 0, len(segs))
	for i := 0; i < len(segs); i++ {
		s := segs[i]
		prev := ""
		if i > 0 {
			prev = segs[i-1]
		}
		switch {
		case i == 1 && (segs[0] == "repos") && len(segs) >= 3:
			out = append(out, "{owner}", "{repo}")
			i++
			continue
		case i == 1 && segs[0] == "users":
			out = append(out, "{username}")
			continue
		case i == 1 && (segs[0] == "orgs" || segs[0] == "organizations" || segs[0] == "enterprises"):
			out = append(out, "{org}")
			continue
		case i > 0 && freeFormTail(segs, i) != "":
			out = append(out, freeFormTail(segs, i)) // rest of the path is free-form (file path, ref, branch, range)
			return "/" + strings.Join(out, "/")
		case prev == "workflows" && i >= 2 && segs[i-2] == "actions":
			out = append(out, "{workflow_id}") // numeric ID or file name (ci.yml)
			continue
		case prev == "branches" || prev == "labels" || prev == "environments":
			out = append(out, "{name}")
			continue
		case reNumeric.MatchString(s):
			out = append(out, "{id}")
			continue
		case reSHA.MatchString(s):
			out = append(out, "{sha}")
			continue
		}
		out = append(out, s)
	}
	return "/" + strings.Join(out, "/")
}

// freeFormTail returns the placeholder for segs[i:] when it is a free-form
// value such as a file path (contents/a/b.md), a git ref
// (git/refs/heads/main), a compare range or a branch name that may contain
// slashes (rules/branches/dependabot/nuget/x-1.0). It returns "" otherwise.
func freeFormTail(segs []string, i int) string {
	prev := segs[i-1]
	switch prev {
	case "contents", "readme", "tarball", "zipball", "compare", "matching-refs":
		return "{path}"
	case "ref", "refs":
		if i >= 2 && segs[i-2] == "git" {
			return "{path}"
		}
	case "branches":
		if i >= 2 && segs[i-2] == "rules" {
			return "{branch}"
		}
	}
	return ""
}

var reGraphQLOp = regexp.MustCompile(`^\s*(query|mutation|subscription)\s*([A-Za-z_][A-Za-z0-9_]*)?`)
var reFirstField = regexp.MustCompile(`\{\s*([A-Za-z_][A-Za-z0-9_]*)`)

// graphQLOperation extracts a readable operation label from a GraphQL body:
// the operationName, else the named operation, else "query:<first field>".
func graphQLOperation(body []byte) string {
	var req struct {
		Query         string `json:"query"`
		OperationName string `json:"operationName"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	if req.OperationName != "" {
		return req.OperationName
	}
	q := stripGraphQLComments(req.Query)
	kind := "query"
	if m := reGraphQLOp.FindStringSubmatch(q); m != nil {
		kind = m[1]
		if m[2] != "" {
			return kind + " " + m[2]
		}
	}
	if m := reFirstField.FindStringSubmatch(q); m != nil {
		return kind + " {" + m[1] + "}"
	}
	return kind
}

func stripGraphQLComments(q string) string {
	var b strings.Builder
	for _, line := range strings.Split(q, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// sanitizeQuery drops credentials that some legacy clients put in the query string.
func sanitizeQuery(raw string) string {
	if raw == "" {
		return ""
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return ""
	}
	for _, k := range []string{"access_token", "client_secret", "token"} {
		if q.Has(k) {
			q.Set(k, "REDACTED")
		}
	}
	return q.Encode()
}

// ClientIP returns the caller address. X-Forwarded-For / X-Real-IP are only
// honoured when the direct peer is a trusted proxy (the local NGINX).
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isTrusted(host, trusted) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := strings.TrimSpace(parts[i])
			if ip != "" && !isTrusted(ip, trusted) {
				return ip
			}
		}
		if ip := strings.TrimSpace(parts[0]); ip != "" {
			return ip
		}
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	return host
}

func isTrusted(ip string, trusted []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// publicBase works out the base URL clients used to reach the proxy.
func publicBase(r *http.Request, configured *url.URL, trusted []*net.IPNet) string {
	if configured != nil {
		return configured.String()
	}
	scheme, host := "http", r.Host
	if r.TLS != nil {
		scheme = "https"
	}
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	if isTrusted(peer, trusted) {
		if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
			scheme = p
		}
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			host = strings.TrimSpace(strings.Split(h, ",")[0])
		}
	}
	return scheme + "://" + host
}
