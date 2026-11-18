package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type allowAllStatus struct{}

func (allowAllStatus) Status() (bool, int) { return true, 0 }

func newTestAPI() *API {
	return New(nil, "token", "1.2.3", time.Now(), allowAllStatus{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestIconsAreServed(t *testing.T) {
	a := newTestAPI()
	cases := []struct {
		path, contentType string
		magic             []byte
	}{
		{"/favicon.ico", "image/x-icon", []byte{0, 0, 1, 0}},
		{"/favicon.svg", "image/svg+xml", []byte("<svg")},
		{"/apple-touch-icon.png", "image/png", []byte("\x89PNG")},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != c.contentType {
				t.Errorf("Content-Type = %q, want %q", got, c.contentType)
			}
			if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "max-age") {
				t.Errorf("Cache-Control = %q, want a cacheable max-age", got)
			}
			if !bytes.HasPrefix(rec.Body.Bytes(), c.magic) {
				t.Errorf("body does not start with %q", c.magic)
			}
		})
	}
}

func TestSwaggerPageLinksIcons(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestAPI().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/swagger/", nil))
	body := rec.Body.String()
	for _, href := range []string{`href="/favicon.ico"`, `href="/favicon.svg"`, `href="/apple-touch-icon.png"`} {
		if !strings.Contains(body, href) {
			t.Errorf("swagger page is missing %s", href)
		}
	}
}
