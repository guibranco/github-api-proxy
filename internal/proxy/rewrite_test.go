package proxy

import (
	"net"
	"net/http/httptest"
	"testing"
)

func TestUpstreamPath(t *testing.T) {
	cases := []struct{ in, path, prefix string }{
		{"/user", "/user", ""},
		{"/api/v3/repos/a/b", "/repos/a/b", "/api/v3"},
		{"/api/v3", "/", "/api/v3"},
		{"/api/graphql", "/graphql", "/api"},
		{"/graphql", "/graphql", ""},
		{"", "/", ""},
	}
	for _, c := range cases {
		p, pre := upstreamPath(c.in)
		if p != c.path || pre != c.prefix {
			t.Errorf("upstreamPath(%q) = %q,%q want %q,%q", c.in, p, pre, c.path, c.prefix)
		}
	}
}

func TestRoute(t *testing.T) {
	cases := map[string]string{
		"/":                                   "/",
		"/user":                               "/user",
		"/repos/guibranco/x/pulls/12":         "/repos/{owner}/{repo}/pulls/{id}",
		"/repos/guibranco/x":                  "/repos/{owner}/{repo}",
		"/repos/a/b/contents/docs/readme.md":  "/repos/{owner}/{repo}/contents/{path}",
		"/repos/a/b/git/refs/heads/feature/x": "/repos/{owner}/{repo}/git/refs/{path}",
		"/repos/a/b/commits/0123456789abcdef0123456789abcdef01234567":                             "/repos/{owner}/{repo}/commits/{sha}",
		"/repos/a/b/compare/main...dev":                                                           "/repos/{owner}/{repo}/compare/{path}",
		"/repos/a/b/branches/main/protection":                                                     "/repos/{owner}/{repo}/branches/{name}/protection",
		"/users/octocat/repos":                                                                    "/users/{username}/repos",
		"/orgs/ApiBR/members":                                                                     "/orgs/{org}/members",
		"/organizations/ApiBR/settings/billing/usage":                                             "/organizations/{org}/settings/billing/usage",
		"/organizations/ApiBR/settings/billing/usage/summary":                                     "/organizations/{org}/settings/billing/usage/summary",
		"/users/guibranco/settings/billing/usage":                                                 "/users/{username}/settings/billing/usage",
		"/repositories/123456":                                                                    "/repositories/{id}",
		"/repos/a/b/rules/branches/main":                                                          "/repos/{owner}/{repo}/rules/branches/{branch}",
		"/repos/a/b/rules/branches/dependabot/nuget/dot-config/csharpier-1.4.0":                   "/repos/{owner}/{repo}/rules/branches/{branch}",
		"/repos/a/b/rules/branches/dependabot/github_actions/github/codeql-action/analyze-4.37.3": "/repos/{owner}/{repo}/rules/branches/{branch}",
		"/repos/a/b/actions/workflows/csharpier.yml/dispatches":                                   "/repos/{owner}/{repo}/actions/workflows/{workflow_id}/dispatches",
		"/repos/a/b/actions/workflows/12345/runs":                                                 "/repos/{owner}/{repo}/actions/workflows/{workflow_id}/runs",
		"/repos/a/b/issues/comments/99/reactions":                                                 "/repos/{owner}/{repo}/issues/comments/{id}/reactions",
		"/graphql":                            "/graphql",
		"/app/installations/42/access_tokens": "/app/installations/{id}/access_tokens",
	}
	for in, want := range cases {
		if got := Route(in); got != want {
			t.Errorf("Route(%q) = %q want %q", in, got, want)
		}
	}
}

func TestRewriteAbsolute(t *testing.T) {
	link := `<https://api.github.com/user/repos?page=2>; rel="next", <https://api.github.com/user/repos?page=5>; rel="last"`
	got := rewriteAbsolute(link, "https://api.github.com", "https://gh.example.com", "")
	want := `<https://gh.example.com/user/repos?page=2>; rel="next", <https://gh.example.com/user/repos?page=5>; rel="last"`
	if got != want {
		t.Fatalf("got %s", got)
	}
	got = rewriteAbsolute("https://api.github.com/repositories/1", "https://api.github.com", "https://gh.example.com", "/api/v3")
	if got != "https://gh.example.com/api/v3/repositories/1" {
		t.Fatalf("prefix not kept: %s", got)
	}
	got = rewriteAbsolute("https://codeload.github.com/a/b/zip", "https://api.github.com", "https://gh.example.com", "")
	if got != "https://codeload.github.com/a/b/zip" {
		t.Fatalf("foreign host rewritten: %s", got)
	}
}

func TestGraphQLOperation(t *testing.T) {
	cases := map[string]string{
		`{"query":"query Viewer { viewer { login } }"}`:                   "query Viewer",
		`{"query":"{ viewer { login } }"}`:                                "query {viewer}",
		`{"query":"mutation { addStar(input:{}) { clientMutationId } }"}`: "mutation {addStar}",
		`{"query":"# c\nquery($o:String!){ repository(owner:$o){id} }"}`:  "query {repository}",
		`{"query":"{ x }","operationName":"MyOp"}`:                        "MyOp",
		`not json`: "",
	}
	for in, want := range cases {
		if got := graphQLOperation([]byte(in)); got != want {
			t.Errorf("graphQLOperation(%s) = %q want %q", in, got, want)
		}
	}
}

func TestSanitizeQuery(t *testing.T) {
	if got := sanitizeQuery("per_page=100&access_token=secret"); got != "access_token=REDACTED&per_page=100" {
		t.Fatalf("got %s", got)
	}
}

func TestClientIP(t *testing.T) {
	_, local, _ := net.ParseCIDR("127.0.0.1/32")
	trusted := []*net.IPNet{local}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if ip := ClientIP(r, trusted); ip != "10.0.0.1" {
		t.Fatalf("got %s", ip)
	}

	r = httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "198.51.100.7:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4") // spoofed by an untrusted peer
	if ip := ClientIP(r, trusted); ip != "198.51.100.7" {
		t.Fatalf("spoofed header honoured: %s", ip)
	}
}
