package grimoire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGrimoire serves consumer/configurations/{key} with ETags and 304s.
type fakeGrimoire struct {
	mu          sync.Mutex
	values      map[string]string
	down        bool
	calls       int
	notModified int
}

func (f *fakeGrimoire) set(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
}

func (f *fakeGrimoire) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.down {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/consumer/configurations/")
	value, ok := f.values[key]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Not Found","detail":"Configuration not found."}`))
		return
	}
	sum := sha256.Sum256([]byte(key + "=" + value))
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		f.notModified++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"key": key, "value": value})
}

func newWatcher(t *testing.T, f *fakeGrimoire) (*Watcher, *int) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	reloads := 0
	w := NewWatcher(New(srv.URL, "k", "production", "test"), nil, time.Hour,
		func(context.Context) error { reloads++; return nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return w, &reloads
}

func TestWatcher_UnchangedSentinelsCost304sAndDoNotReload(t *testing.T) {
	f := &fakeGrimoire{values: map[string]string{"Sentinel": "100", "Global:Sentinel": "5"}}
	w, reloads := newWatcher(t, f)
	if err := w.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}

	changed, err := w.Check(context.Background())

	if err != nil || changed || *reloads != 0 {
		t.Fatalf("changed=%v err=%v reloads=%d", changed, err, *reloads)
	}
	if f.notModified != 2 {
		t.Fatalf("want 2 conditional hits, got %d", f.notModified)
	}
}

func TestWatcher_ChangedSentinelReloads(t *testing.T) {
	f := &fakeGrimoire{values: map[string]string{"Sentinel": "100"}}
	w, reloads := newWatcher(t, f)
	_ = w.Prime(context.Background())

	f.set("Sentinel", "101")
	changed, err := w.Check(context.Background())

	if err != nil || !changed || *reloads != 1 {
		t.Fatalf("changed=%v err=%v reloads=%d", changed, err, *reloads)
	}
	if changed, _ := w.Check(context.Background()); changed {
		t.Fatal("the same change must not reload twice")
	}
}

func TestWatcher_SentinelAppearingReloads(t *testing.T) {
	f := &fakeGrimoire{values: map[string]string{}}
	w, reloads := newWatcher(t, f)
	_ = w.Prime(context.Background())

	f.set("Global:Sentinel", "1")

	if changed, err := w.Check(context.Background()); err != nil || !changed || *reloads != 1 {
		t.Fatalf("changed=%v err=%v reloads=%d", changed, err, *reloads)
	}
}

func TestWatcher_FailedReloadIsRetried(t *testing.T) {
	f := &fakeGrimoire{values: map[string]string{"Sentinel": "100"}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	fail := true
	reloads := 0
	w := NewWatcher(New(srv.URL, "k", "production", "test"), nil, time.Hour, func(context.Context) error {
		if fail {
			return errors.New("reload failed")
		}
		reloads++
		return nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_ = w.Prime(context.Background())

	f.set("Sentinel", "101")
	if _, err := w.Check(context.Background()); err == nil {
		t.Fatal("want the reload error")
	}
	fail = false
	if changed, err := w.Check(context.Background()); err != nil || !changed || reloads != 1 {
		t.Fatalf("changed=%v err=%v reloads=%d", changed, err, reloads)
	}
}

func TestWatcher_WithoutBaselineTheFirstCheckReloadsOnce(t *testing.T) {
	f := &fakeGrimoire{values: map[string]string{"Sentinel": "100"}, down: true}
	w, reloads := newWatcher(t, f)
	if err := w.Prime(context.Background()); err == nil {
		t.Fatal("want a prime error")
	}

	f.mu.Lock()
	f.down = false
	f.mu.Unlock()
	if changed, err := w.Check(context.Background()); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if changed, _ := w.Check(context.Background()); changed || *reloads != 1 {
		t.Fatalf("changed=%v reloads=%d", changed, *reloads)
	}
}

func TestWatcher_GrimoireDownIsAnError(t *testing.T) {
	f := &fakeGrimoire{values: map[string]string{"Sentinel": "100"}}
	w, reloads := newWatcher(t, f)
	_ = w.Prime(context.Background())
	f.mu.Lock()
	f.down = true
	f.mu.Unlock()

	if _, err := w.Check(context.Background()); err == nil || *reloads != 0 {
		t.Fatalf("err=%v reloads=%d", err, *reloads)
	}
}

func TestWatcher_NextDelay(t *testing.T) {
	w := &Watcher{interval: 10 * time.Second}
	for range 50 {
		if d := w.nextDelay(0); d < 10*time.Second || d > 12*time.Second {
			t.Fatalf("jittered delay out of range: %v", d)
		}
	}
	if d := w.nextDelay(1); d != 20*time.Second {
		t.Fatalf("got %v", d)
	}
	if d := w.nextDelay(10); d != maxBackoff {
		t.Fatalf("got %v", d)
	}
}
