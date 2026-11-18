// Package store persists proxied requests and caller identities in MySQL /
// MariaDB, and answers the stats queries behind the REST API.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/guibranco/github-api-proxy/internal/identity"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Record is one proxied request.
type Record struct {
	RequestedAt        time.Time
	Duration           time.Duration
	Method             string
	Path               string
	Route              string
	Query              string
	GraphQLOperation   string
	Status             int
	RequestBytes       int64
	ResponseBytes      int64
	ClientIP           string
	UserAgent          string
	TokenHash          string
	TokenKind          string
	RateLimitResource  string
	RateLimitLimit     int // -1 when absent
	RateLimitRemaining int
	RateLimitUsed      int
	RateLimitReset     time.Time
	GitHubRequestID    string
	Error              string
}

// Store wraps the database and the asynchronous request writer.
type Store struct {
	db  *sql.DB
	log *slog.Logger

	queue     chan Record
	batchSize int
	interval  time.Duration
	retention int

	dropped      atomic.Int64
	droppedTotal atomic.Int64
	pending      atomic.Int64
	failing      atomic.Bool
	lastError    atomic.Value // string
	done         chan struct{}
}

// Open connects to MySQL, forcing UTC and parseTime regardless of the DSN.
func Open(dsn string, queueSize, batchSize int, interval time.Duration, retentionDays int, log *slog.Logger) (*Store, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid MySql--ConnectionString (expected user:pass@tcp(host:3306)/db): %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 30 * time.Second
	}

	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute) // shared hosts close idle connections aggressively

	s := &Store{
		db:        db,
		log:       log,
		queue:     make(chan Record, queueSize),
		batchSize: batchSize,
		interval:  interval,
		retention: retentionDays,
		done:      make(chan struct{}),
	}
	s.lastError.Store("")
	return s, nil
}

// DB exposes the pool for the API queries.
func (s *Store) DB() *sql.DB { return s.db }

// Ping checks connectivity.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Migrate applies the embedded migrations that have not been applied yet.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS github_proxy_migrations (
		version VARCHAR(100) NOT NULL PRIMARY KEY,
		applied_at DATETIME(3) NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		var exists int
		err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM github_proxy_migrations WHERE version = ?", e.Name()).Scan(&exists)
		if err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		raw, err := migrationFiles.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		for _, stmt := range splitSQL(string(raw)) {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("%s: %w", e.Name(), err)
			}
		}
		if _, err := s.db.ExecContext(ctx, "INSERT INTO github_proxy_migrations (version, applied_at) VALUES (?, UTC_TIMESTAMP(3))", e.Name()); err != nil {
			return err
		}
		s.log.Info("Applied database migration", "category", "startup", "migration", e.Name())
	}
	return nil
}

func splitSQL(script string) []string {
	var lines []string
	for _, l := range strings.Split(script, "\n") {
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		lines = append(lines, l)
	}
	var out []string
	for _, stmt := range strings.Split(strings.Join(lines, "\n"), ";") {
		if s := strings.TrimSpace(stmt); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Asynchronous writer
// ---------------------------------------------------------------------------

// Enqueue never blocks: when MySQL is slow or down and the queue is full, the
// record is dropped and counted, and the proxied request is unaffected.
func (s *Store) Enqueue(r Record) {
	select {
	case s.queue <- r:
		s.pending.Add(1)
	default:
		s.dropped.Add(1)
	}
}

// Stats about the writer, for /health.
type WriterStats struct {
	Queued    int64  `json:"queued"`
	Dropped   int64  `json:"dropped_since_start"`
	Failing   bool   `json:"failing"`
	LastError string `json:"last_error,omitempty"`
}

// Stats returns the writer state.
func (s *Store) Stats() WriterStats {
	return WriterStats{
		Queued:    s.pending.Load(),
		Dropped:   s.droppedTotal.Load() + s.dropped.Load(),
		Failing:   s.failing.Load(),
		LastError: s.lastError.Load().(string),
	}
}

// Run writes queued records until the queue is closed by Close.
func (s *Store) Run(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	batch := make([]Record, 0, s.batchSize)

	flush := func(final bool) {
		if n := s.dropped.Swap(0); n > 0 {
			s.droppedTotal.Add(n)
			s.log.Error("Request records dropped: the MySQL write queue was full", "category", "storage", "dropped", n)
		}
		backoff := time.Second
		for len(batch) > 0 {
			err := s.insert(context.Background(), batch)
			if err == nil {
				if s.failing.Swap(false) {
					s.log.Info("MySQL writes recovered", "category", "storage")
				}
				s.lastError.Store("")
				s.pending.Add(-int64(len(batch)))
				batch = batch[:0]
				return
			}
			s.lastError.Store(err.Error())
			if !s.failing.Swap(true) {
				s.log.Error("Writing request records to MySQL failed, retrying", "category", "storage", "error", err, "batch", len(batch))
			}
			if final || ctx.Err() != nil {
				s.log.Error("Giving up on request records at shutdown", "category", "storage", "lost", len(batch))
				s.pending.Add(-int64(len(batch)))
				batch = batch[:0]
				return
			}
			time.Sleep(backoff)
			backoff = min(backoff*2, 30*time.Second)
		}
	}

	for {
		select {
		case r, ok := <-s.queue:
			if !ok {
				flush(true)
				return
			}
			batch = append(batch, r)
			if len(batch) >= s.batchSize {
				flush(false)
			}
		case <-ticker.C:
			flush(false)
		}
	}
}

// Close stops accepting records and waits for the writer to drain.
func (s *Store) Close(ctx context.Context) {
	close(s.queue)
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	_ = s.db.Close()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int) any {
	if n < 0 {
		return nil
	}
	return n
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// avoid cutting a UTF-8 sequence in half
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func (s *Store) insert(ctx context.Context, batch []Record) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	const cols = 21
	var sb strings.Builder
	sb.WriteString(`INSERT INTO github_proxy_requests
		(requested_at, duration_ms, method, path, route, query, graphql_operation, status,
		 request_bytes, response_bytes, client_ip, user_agent, token_hash, token_kind,
		 rate_limit_resource, rate_limit_limit, rate_limit_remaining, rate_limit_used, rate_limit_reset,
		 github_request_id, error) VALUES `)
	args := make([]any, 0, len(batch)*cols)

	type seen struct {
		kind        string
		first, last time.Time
	}
	tokens := map[string]*seen{}

	for i, r := range batch {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
		var reset any
		if !r.RateLimitReset.IsZero() {
			reset = r.RateLimitReset.UTC()
		}
		args = append(args,
			r.RequestedAt.UTC(), r.Duration.Milliseconds(), r.Method, clip(r.Path, 1024), clip(r.Route, 255),
			nullStr(clip(r.Query, 1024)), nullStr(clip(r.GraphQLOperation, 255)), r.Status,
			r.RequestBytes, r.ResponseBytes, r.ClientIP, nullStr(clip(r.UserAgent, 255)),
			nullStr(r.TokenHash), r.TokenKind,
			nullStr(r.RateLimitResource), nullInt(r.RateLimitLimit), nullInt(r.RateLimitRemaining), nullInt(r.RateLimitUsed), reset,
			nullStr(clip(r.GitHubRequestID, 64)), nullStr(clip(r.Error, 512)),
		)
		if r.TokenHash != "" {
			t, ok := tokens[r.TokenHash]
			if !ok {
				tokens[r.TokenHash] = &seen{kind: r.TokenKind, first: r.RequestedAt, last: r.RequestedAt}
			} else if r.RequestedAt.After(t.last) {
				t.last = r.RequestedAt
			}
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return err
	}
	for hash, t := range tokens {
		if _, err := tx.ExecContext(ctx, `INSERT INTO github_proxy_tokens (token_hash, token_kind, first_seen_at, last_seen_at)
			VALUES (?,?,?,?)
			ON DUPLICATE KEY UPDATE last_seen_at = GREATEST(last_seen_at, VALUES(last_seen_at))`,
			hash, t.kind, t.first.UTC(), t.last.UTC()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

// RunRetention deletes old rows every hour, in small chunks to keep locks short.
func (s *Store) RunRetention(ctx context.Context) {
	run := func() {
		total := int64(0)
		for {
			res, err := s.db.ExecContext(ctx,
				"DELETE FROM github_proxy_requests WHERE requested_at < UTC_TIMESTAMP() - INTERVAL ? DAY LIMIT 5000", s.retention)
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("Retention cleanup failed", "category", "retention", "error", err)
				}
				return
			}
			n, _ := res.RowsAffected()
			total += n
			if n < 5000 {
				break
			}
		}
		res, err := s.db.ExecContext(ctx,
			"DELETE FROM github_proxy_tokens WHERE last_seen_at < UTC_TIMESTAMP() - INTERVAL ? DAY", s.retention)
		if err == nil {
			n, _ := res.RowsAffected()
			if total > 0 || n > 0 {
				s.log.Info("Retention cleanup", "category", "retention", "requests_deleted", total, "tokens_deleted", n, "retention_days", s.retention)
			}
		}
	}
	run()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// ---------------------------------------------------------------------------
// identity.Store
// ---------------------------------------------------------------------------

// LookupIdentity implements identity.Store.
func (s *Store) LookupIdentity(ctx context.Context, hash string) (string, error) {
	var caller sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT caller FROM github_proxy_tokens WHERE token_hash = ? AND resolved_at IS NOT NULL", hash).Scan(&caller)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return caller.String, err
}

// SaveIdentity implements identity.Store.
func (s *Store) SaveIdentity(ctx context.Context, hash, kind string, id identity.Identity, resolveErr string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO github_proxy_tokens
		(token_hash, token_kind, caller, login, account_type, note, resolve_error, first_seen_at, last_seen_at, resolved_at)
		VALUES (?,?,?,?,?,?,?,UTC_TIMESTAMP(3),UTC_TIMESTAMP(3),UTC_TIMESTAMP(3))
		ON DUPLICATE KEY UPDATE caller = VALUES(caller), login = VALUES(login), account_type = VALUES(account_type),
			note = VALUES(note), resolve_error = VALUES(resolve_error), resolved_at = VALUES(resolved_at)`,
		hash, kind, clip(id.Caller, 255), nullStr(clip(id.Login, 255)), nullStr(clip(id.AccountType, 20)),
		nullStr(clip(id.Note, 255)), nullStr(clip(resolveErr, 255)))
	return err
}
