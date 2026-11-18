package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guibranco/github-api-proxy/internal/config"
	"github.com/guibranco/github-api-proxy/internal/identity"
	"github.com/guibranco/github-api-proxy/internal/store"
)

type fakeRecorder struct {
	mu   sync.Mutex
	recs []store.Record
}

func (f *fakeRecorder) Enqueue(r store.Record) {
	f.mu.Lock()
	f.recs = append(f.recs, r)
	f.mu.Unlock()
}
func (f *fakeRecorder) last() store.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recs[len(f.recs)-1]
}

type fakeObserver struct{}

func (fakeObserver) Observe(c identity.Credential) string { return "test" }

func setup(t *testing.T, upstream http.HandlerFunc) (*httptest.Server, *fakeRecorder) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	u, _ := url.Parse(up.URL)
	cfg := &config.Config{UpstreamURL: u, UpstreamTimeout: 5 * time.Second, RateLimitWarnPercent: 10}
	rec := &fakeRecorder{}
	h := New(cfg, rec, fakeObserver{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	px := httptest.NewServer(h)
	t.Cleanup(px.Close)
	return px, rec
}

func TestProxyForwardsAndRecords(t *testing.T) {
	var gotAuth, gotPath, gotHost, gotXFF string
	px, rec := setup(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotHost, gotXFF = r.Header.Get("Authorization"), r.URL.Path, r.Host, r.Header.Get("X-Forwarded-For")
		self := "http://" + r.Host
		w.Header().Set("Link", "<"+self+"/user/repos?page=2>; rel=\"next\"")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Used", "679")
		w.Header().Set("X-RateLimit-Reset", "1900000000")
		w.Header().Set("X-RateLimit-Resource", "core")
		w.Header().Set("X-GitHub-Request-Id", "ABCD:1234")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":1}]`)
	})

	req, _ := http.NewRequest("GET", px.URL+"/api/v3/user/repos?per_page=1", nil)
	req.Header.Set("Authorization", "token ghp_example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != `[{"id":1}]` || resp.StatusCode != 200 {
		t.Fatalf("unexpected response %d %s", resp.StatusCode, body)
	}
	if gotAuth != "token ghp_example" {
		t.Fatalf("authorization not forwarded: %q", gotAuth)
	}
	if gotPath != "/user/repos" {
		t.Fatalf("/api/v3 not stripped: %q", gotPath)
	}
	if gotXFF != "" {
		t.Fatalf("proxy chain leaked upstream: %q", gotXFF)
	}
	if !strings.HasPrefix(gotHost, "127.0.0.1") {
		t.Fatalf("host not rewritten: %q", gotHost)
	}
	if link := resp.Header.Get("Link"); !strings.Contains(link, px.URL+"/api/v3/user/repos?page=2") {
		t.Fatalf("Link not rewritten: %s", link)
	}

	r := rec.last()
	if r.Status != 200 || r.Method != "GET" || r.Path != "/user/repos" || r.Route != "/user/repos" {
		t.Fatalf("bad record: %+v", r)
	}
	if r.TokenKind != identity.KindClassicPAT || len(r.TokenHash) != 64 {
		t.Fatalf("token not fingerprinted: %+v", r)
	}
	if r.RateLimitRemaining != 4321 || r.RateLimitLimit != 5000 || r.RateLimitResource != "core" || r.RateLimitReset.Unix() != 1900000000 {
		t.Fatalf("rate limit not captured: %+v", r)
	}
	if r.ResponseBytes != int64(len(body)) || r.GitHubRequestID != "ABCD:1234" || r.Query != "per_page=1" {
		t.Fatalf("bad record details: %+v", r)
	}
}

func TestProxyGraphQL(t *testing.T) {
	var gotBody string
	px, rec := setup(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"data":{}}`)
	})
	payload := `{"query":"query Repos { viewer { repositories(first: 1) { totalCount } } }"}`
	resp, err := http.Post(px.URL+"/graphql", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotBody != payload {
		t.Fatalf("body altered: %s", gotBody)
	}
	r := rec.last()
	if r.GraphQLOperation != "query Repos" || r.RequestBytes != int64(len(payload)) {
		t.Fatalf("bad record: %+v", r)
	}
}

func TestProxyUpstreamDown(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1") // nothing listens here
	cfg := &config.Config{UpstreamURL: u, UpstreamTimeout: time.Second}
	rec := &fakeRecorder{}
	px := httptest.NewServer(New(cfg, rec, fakeObserver{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer px.Close()

	resp, err := http.Get(px.URL + "/rate_limit")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if r := rec.last(); r.Status != 502 || r.Error == "" || r.TokenKind != identity.KindNone {
		t.Fatalf("bad record: %+v", r)
	}
}
