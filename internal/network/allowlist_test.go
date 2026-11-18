package network

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeResolver map[string][]string

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	var out []net.IPAddr
	for _, ip := range ips {
		out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
	}
	return out, nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestAllowList(t *testing.T) {
	raw := "node1.example.com, node2.example.com\n198.51.100.30 # cPanel host\n10.8.0.0/24"
	dns := fakeResolver{"node1.example.com": {"192.0.2.10", "2603:c020::1"}, "node2.example.com": {"192.0.2.20"}}
	a := New(func(context.Context) (string, error) { return raw, nil }, dns, quiet)
	if err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]bool{
		"192.0.2.10": true, "2603:c020::1": true, "192.0.2.20": true, "198.51.100.30": true,
		"10.8.0.7": true, "127.0.0.1": true, "::1": true,
		"8.8.8.8": false, "10.8.1.1": false, "garbage": false,
	} {
		if got := a.Allowed(ip); got != want {
			t.Errorf("Allowed(%s) = %v want %v", ip, got, want)
		}
	}

	// DNS failure keeps the last good addresses; a source error keeps the whole list.
	delete(dns, "node2.example.com")
	_ = a.Refresh(context.Background())
	if !a.Allowed("192.0.2.20") {
		t.Fatal("last good resolution should be kept")
	}
	a.source = func(context.Context) (string, error) { return "", errors.New("grimoire down") }
	if err := a.Refresh(context.Background()); err != nil || !a.Allowed("198.51.100.30") {
		t.Fatalf("previous list should stay in force: %v", err)
	}

	// The list changes live, e.g. a new home IP in Grimoire.
	a.source = func(context.Context) (string, error) { return "203.0.113.50", nil }
	_ = a.Refresh(context.Background())
	if !a.Allowed("203.0.113.50") || a.Allowed("198.51.100.30") {
		t.Fatal("refresh did not replace the list")
	}
}

func TestAllowAllAndInitialFailure(t *testing.T) {
	a := New(func(context.Context) (string, error) { return "*", nil }, fakeResolver{}, quiet)
	_ = a.Refresh(context.Background())
	if !a.Allowed("8.8.8.8") {
		t.Fatal("* should allow everyone")
	}
	b := New(func(context.Context) (string, error) { return "", errors.New("down") }, fakeResolver{}, quiet)
	if err := b.Refresh(context.Background()); err == nil {
		t.Fatal("first load must fail when the source fails")
	}
}

func TestMiddleware(t *testing.T) {
	a := New(func(context.Context) (string, error) { return "198.51.100.0/24", nil }, fakeResolver{}, quiet)
	_ = a.Refresh(context.Background())
	var denied string
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }),
		func(r *http.Request) string { return r.Header.Get("X-Test-IP") },
		func(r *http.Request) bool { return r.URL.Path == "/health" },
		func(_ *http.Request, ip string) { denied = ip })

	for _, c := range []struct {
		ip, path string
		want     int
	}{{"198.51.100.9", "/user", 204}, {"8.8.8.8", "/user", 403}, {"8.8.8.8", "/health", 204}} {
		r := httptest.NewRequest("GET", c.path, nil)
		r.Header.Set("X-Test-IP", c.ip)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s %s: got %d want %d", c.ip, c.path, w.Code, c.want)
		}
	}
	if denied != "8.8.8.8" {
		t.Fatalf("onDenied not called: %q", denied)
	}
}

func TestSplitComments(t *testing.T) {
	got := split("1.2.3.4 # osasco, ignored\nnode1.example.com;10.0.0.0/8\n# whole line")
	want := []string{"1.2.3.4", "node1.example.com", "10.0.0.0/8"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v", got)
		}
	}
}
