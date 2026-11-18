package logstream

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guibranco/github-api-proxy/internal/config"
)

func TestHTTPTransportShipsBatches(t *testing.T) {
	stderr = io.Discard
	var mu sync.Mutex
	var got []batch
	var key, token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		got = append(got, b)
		key, token = r.Header.Get("X-Api-Key"), r.Header.Get("X-Api-Token")
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := New(config.LogStreamConfig{Enabled: true, ServerURL: srv.URL, APIKey: "github-api-proxy", APIToken: "tok",
		AppID: "1.2.3", Transport: "http"}, "test/1.0")
	log := slog.New(NewHandler(c, "info"))
	log.Debug("not shipped")
	log.Info("started", "category", "startup", "version", "1.2.3")
	log.Warn("low", "category", "rate-limit", "remaining", 12)
	log.Error("boom", "category", "upstream", "error", io.ErrUnexpectedEOF)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Close(ctx)

	mu.Lock()
	defer mu.Unlock()
	if key != "github-api-proxy" || token != "tok" {
		t.Fatalf("auth headers: %q %q", key, token)
	}
	var logs []entry
	for _, b := range got {
		if b.AppKey != "github-api-proxy" || b.AppID != "1.2.3" || b.BatchID == "" {
			t.Fatalf("bad batch envelope: %+v", b)
		}
		logs = append(logs, b.Logs...)
	}
	if len(logs) != 3 {
		t.Fatalf("want 3 entries, got %d: %+v", len(logs), logs)
	}
	if logs[0].Level != "info" || logs[0].Category != "startup" || logs[0].Context["version"] != "1.2.3" {
		t.Fatalf("bad entry: %+v", logs[0])
	}
	if logs[1].Level != "warning" || logs[2].Level != "error" || logs[2].Context["error"] != "unexpected EOF" {
		t.Fatalf("bad levels/context: %+v", logs)
	}
	if logs[0].TraceID == "" || logs[0].TraceID != logs[2].TraceID {
		t.Fatal("entries of one process must share a trace_id")
	}
}

// verify checks a datagram exactly as logstream-server does.
func verify(t *testing.T, packet []byte, secret string) map[string]any {
	t.Helper()
	var p struct {
		V     int             `json:"v"`
		Key   string          `json:"key"`
		TS    int64           `json:"ts"`
		Nonce string          `json:"nonce"`
		Sig   string          `json:"sig"`
		Log   json.RawMessage `json:"log"`
	}
	if err := json.Unmarshal(packet, &p); err != nil {
		t.Fatalf("invalid packet %s: %v", packet, err)
	}
	// the server signs the log text exactly as received in the packet
	start := strings.Index(string(packet), `"log":`) + len(`"log":`)
	logText := string(packet[start : len(packet)-1])
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(p.Key + ":" + strconv.FormatInt(p.TS, 10) + ":" + p.Nonce + ":" + logText))
	if want := hex.EncodeToString(mac.Sum(nil)); want != p.Sig {
		t.Fatalf("bad signature")
	}
	if p.V != 1 || p.Key != "github-api-proxy" || len(p.Nonce) < 16 || time.Since(time.UnixMilli(p.TS)) > 5*time.Second {
		t.Fatalf("bad packet header: %+v", p)
	}
	var l map[string]any
	_ = json.Unmarshal(p.Log, &l)
	return l
}

func TestUDPTransportSignsPacketsAndFallsBackToHTTP(t *testing.T) {
	stderr = io.Discard
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	port := pc.LocalAddr().(*net.UDPAddr).Port

	var httpEntries []entry
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b batch
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		httpEntries = append(httpEntries, b.Logs...)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := New(config.LogStreamConfig{Enabled: true, ServerURL: srv.URL, APIKey: "github-api-proxy", APIToken: "lss_secret",
		AppID: "2.0.1", Transport: "udp", UDPHost: "127.0.0.1", UDPPort: port, UDPMaxPacket: 1200}, "github-api-proxy/2.0.1")
	log := slog.New(NewHandler(c, "info"))
	log.Info("github-api-proxy started", "category", "startup", "path", "/a/b<c>")
	log.Warn("huge", "category", "test", "blob", strings.Repeat("x", 3000)) // > UDPMaxPacket → HTTP

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Close(ctx)

	buf := make([]byte, 65535)
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no datagram: %v", err)
	}
	l := verify(t, buf[:n], "lss_secret")
	if l["app_id"] != "2.0.1" || l["message"] != "github-api-proxy started" || l["category"] != "startup" ||
		l["user_agent"] != "github-api-proxy/2.0.1" || l["trace_id"] == "" || l["batch_id"] == "" {
		t.Fatalf("bad log: %+v", l)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(httpEntries) != 1 || httpEntries[0].Message != "huge" {
		t.Fatalf("oversized entry should go over HTTP, got %+v", httpEntries)
	}
}
