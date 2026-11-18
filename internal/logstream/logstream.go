// Package logstream ships the proxy's own events (startup, shutdown, warnings,
// errors) to a LogStream server (guibranco/logstream-server).
//
// By default each entry is an HMAC-signed UDP datagram (LogStream's UDP
// ingest, default port 8082): fire-and-forget, no TLS handshake, and the API
// token only signs packets, it never travels on the wire. Entries too large
// for one datagram (and everything when LogStream:Transport=Http) are posted
// in batches to POST /api/logs with X-Api-Key / X-Api-Token.
//
// It is exposed as a slog.Handler that writes every record to the console
// (journald / docker logs) and forwards records at or above the configured
// level. Delivery never blocks the caller: when the queue is full, entries are
// dropped and counted.
package logstream

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guibranco/github-api-proxy/internal/config"
)

const (
	queueSize     = 2000
	maxBatch      = 100
	flushInterval = 2 * time.Second
	maxAttempts   = 4
)

type entry struct {
	TraceID   string         `json:"trace_id"`
	Level     string         `json:"level"`
	Category  string         `json:"category"`
	Message   string         `json:"message"`
	Context   map[string]any `json:"context,omitempty"`
	Timestamp string         `json:"timestamp"`
}

// udpLog is the "log" object of a UDP packet: one entry plus the fields that
// the HTTP batch envelope would otherwise carry.
type udpLog struct {
	AppID     string `json:"app_id"`
	BatchID   string `json:"batch_id"`
	UserAgent string `json:"user_agent"`
	entry
}

type batch struct {
	AppKey  string  `json:"app_key"`
	AppID   string  `json:"app_id"`
	BatchID string  `json:"batch_id"`
	Logs    []entry `json:"logs"`
}

// Client delivers entries to LogStream in the background.
type Client struct {
	cfg       config.LogStreamConfig
	userAgent string
	traceID   string
	http      *http.Client
	udp       net.Conn
	queue     chan entry
	dropped   atomic.Int64
	done      chan struct{}
	closeOnce sync.Once
	now       func() time.Time
}

// New starts a client. It returns nil when LogStream is not configured.
func New(cfg config.LogStreamConfig, userAgent string) *Client {
	if !cfg.Ready() {
		return nil
	}
	c := &Client{
		cfg:       cfg,
		userAgent: userAgent,
		// Every entry of one process shares a trace ID, so a whole run from
		// startup to shutdown can be pulled up by trace in the LogStream UI
		// (same convention as logstream-server's own self-logging).
		traceID: newUUID(),
		http:    &http.Client{Timeout: 10 * time.Second},
		queue:   make(chan entry, queueSize),
		done:    make(chan struct{}),
		now:     time.Now,
	}
	go c.run()
	return c
}

// TraceID is the per-process trace ID attached to every entry.
func (c *Client) TraceID() string { return c.traceID }

// Transport describes how entries are sent, for the startup log.
func (c *Client) Transport() string {
	if c.cfg.Transport == "udp" {
		return fmt.Sprintf("udp %s (oversized entries over https)", net.JoinHostPort(c.cfg.UDPHost, strconv.Itoa(c.cfg.UDPPort)))
	}
	return "https " + c.cfg.ServerURL
}

func (c *Client) enqueue(e entry) {
	e.TraceID = c.traceID
	select {
	case c.queue <- e:
	default:
		c.dropped.Add(1)
	}
}

// Close flushes pending entries, waiting at most until ctx is done.
func (c *Client) Close(ctx context.Context) {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() { close(c.queue) })
	select {
	case <-c.done:
	case <-ctx.Done():
	}
	if c.udp != nil {
		_ = c.udp.Close()
	}
}

func (c *Client) run() {
	defer close(c.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	var pending []entry
	flush := func() {
		if n := c.dropped.Swap(0); n > 0 {
			pending = append(pending, entry{
				TraceID: c.traceID, Level: "warning", Category: "logstream",
				Message:   fmt.Sprintf("%d LogStream entries were dropped because the queue was full", n),
				Timestamp: c.now().UTC().Format(time.RFC3339Nano),
			})
		}
		for len(pending) > 0 {
			n := min(len(pending), maxBatch)
			c.send(pending[:n])
			pending = pending[n:]
		}
	}

	for {
		select {
		case e, ok := <-c.queue:
			if !ok {
				flush()
				return
			}
			pending = append(pending, e)
			if len(pending) >= maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (c *Client) send(logs []entry) {
	batchID := newUUID()
	if c.cfg.Transport == "udp" {
		var oversized []entry
		for _, e := range logs {
			packet, err := c.packet(e, batchID)
			if err != nil {
				continue
			}
			if len(packet) > c.cfg.UDPMaxPacket {
				oversized = append(oversized, e) // typically long contexts: send over HTTPS instead
				continue
			}
			if err := c.sendUDP(packet); err != nil {
				consoleWarn("LogStream UDP send failed, entry dropped", "error", err.Error())
			}
		}
		logs = oversized
	}
	if len(logs) > 0 {
		c.sendHTTP(logs, batchID)
	}
}

// packet builds a signed datagram: the log JSON is encoded once, signed, and
// embedded byte-for-byte, as the server verifies the exact text it receives.
func (c *Client) packet(e entry, batchID string) ([]byte, error) {
	logJSON, err := json.Marshal(udpLog{AppID: c.cfg.AppID, BatchID: batchID, UserAgent: c.userAgent, entry: e})
	if err != nil {
		return nil, err
	}
	key := c.cfg.APIKey
	ts := strconv.FormatInt(c.now().UnixMilli(), 10)
	nonce := randomHex(8)
	mac := hmac.New(sha256.New, []byte(c.cfg.APIToken))
	mac.Write([]byte(key + ":" + ts + ":" + nonce + ":"))
	mac.Write(logJSON)
	sig := hex.EncodeToString(mac.Sum(nil))

	keyJSON, _ := json.Marshal(key)
	var b bytes.Buffer
	b.Grow(len(logJSON) + 200)
	b.WriteString(`{"v":1,"key":`)
	b.Write(keyJSON)
	b.WriteString(`,"ts":` + ts + `,"nonce":"` + nonce + `","sig":"` + sig + `","log":`)
	b.Write(logJSON)
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (c *Client) sendUDP(packet []byte) error {
	if c.udp == nil {
		conn, err := net.Dial("udp", net.JoinHostPort(c.cfg.UDPHost, strconv.Itoa(c.cfg.UDPPort)))
		if err != nil {
			return err
		}
		c.udp = conn
	}
	if _, err := c.udp.Write(packet); err != nil {
		_ = c.udp.Close()
		c.udp = nil // re-resolve and reconnect on the next packet
		return err
	}
	return nil
}

func (c *Client) sendHTTP(logs []entry, batchID string) {
	body, err := json.Marshal(batch{AppKey: c.cfg.APIKey, AppID: c.cfg.AppID, BatchID: batchID, Logs: logs})
	if err != nil {
		return
	}
	backoff := 500 * time.Millisecond
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		retry, err := c.post(body)
		if err == nil {
			return
		}
		if !retry || attempt == maxAttempts {
			// Console only: logging this to LogStream would loop.
			consoleWarn("LogStream delivery failed, batch dropped", "entries", len(logs), "error", err.Error())
			return
		}
		time.Sleep(backoff)
		backoff *= 2
	}
}

func (c *Client) post(body []byte) (retry bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.cfg.ServerURL+"/api/logs", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("X-Api-Key", c.cfg.APIKey)
	req.Header.Set("X-Api-Token", c.cfg.APIToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK:
		return false, nil
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, fmt.Errorf("LogStream answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	default:
		return false, fmt.Errorf("LogStream rejected the batch with %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
}

func consoleWarn(msg string, args ...any) {
	slog.New(consoleHandler(slog.LevelWarn)).Warn(msg, append([]any{"category", "logstream"}, args...)...)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// slog integration
// ---------------------------------------------------------------------------

// Handler writes to the console and forwards to LogStream.
type Handler struct {
	console  slog.Handler
	client   *Client
	minLevel slog.Level
	attrs    []slog.Attr
	group    string
}

// NewHandler builds the process logger handler. client may be nil (console only).
func NewHandler(client *Client, minLevel string) *Handler {
	return &Handler{
		console:  consoleHandler(slog.LevelDebug),
		client:   client,
		minLevel: ParseLevel(minLevel),
	}
}

func consoleHandler(level slog.Level) slog.Handler {
	return slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})
}

// ParseLevel maps LogStream level names to slog levels.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warning", "warn":
		return slog.LevelWarn
	case "error", "critical":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.console.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	_ = h.console.Handle(ctx, r)
	if h.client == nil || r.Level < h.minLevel {
		return nil
	}

	e := entry{
		Level:     levelName(r.Level),
		Category:  "general",
		Message:   r.Message,
		Timestamp: r.Time.UTC().Format(time.RFC3339Nano),
		Context:   map[string]any{},
	}
	add := func(prefix string, a slog.Attr) {
		key := a.Key
		if prefix != "" {
			key = prefix + "." + key
		}
		if key == "category" {
			e.Category = truncate(a.Value.String(), 100)
			return
		}
		e.Context[key] = attrValue(a.Value)
	}
	for _, a := range h.attrs {
		add("", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		add(h.group, a)
		return true
	})
	if len(e.Context) == 0 {
		e.Context = nil
	}
	h.client.enqueue(e)
	return nil
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.console = h.console.WithAttrs(attrs)
	clone.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &clone
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.console = h.console.WithGroup(name)
	if clone.group != "" {
		clone.group += "." + name
	} else {
		clone.group = name
	}
	return &clone
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError+4:
		return "critical"
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warning"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}

func attrValue(v slog.Value) any {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		m := map[string]any{}
		for _, a := range v.Group() {
			m[a.Key] = attrValue(a.Value)
		}
		return m
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return err.Error()
		}
		return v.Any()
	default:
		return v.Any()
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
