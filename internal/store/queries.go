package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// BlockedError marks requests refused by the network allow-list.
const BlockedError = "blocked by network allow-list"

// callerExpr labels a request row: the resolved caller, else the token
// fingerprint, else "anonymous".
const callerExpr = `COALESCE(t.caller, IF(r.token_hash IS NULL, 'anonymous', CONCAT('token:', LEFT(r.token_hash, 12))))`

const fromJoin = ` FROM github_proxy_requests r LEFT JOIN github_proxy_tokens t ON t.token_hash = r.token_hash `

// Summary is the headline numbers for a period.
type Summary struct {
	Requests          int64            `json:"requests"`
	ClientErrors      int64            `json:"client_errors"`
	ServerErrors      int64            `json:"server_errors"`
	UpstreamFailures  int64            `json:"upstream_failures"`
	RateLimited       int64            `json:"rate_limited"`
	Blocked           int64            `json:"blocked"`
	AvgDurationMs     float64          `json:"avg_duration_ms"`
	MaxDurationMs     int64            `json:"max_duration_ms"`
	RequestBytes      int64            `json:"request_bytes"`
	ResponseBytes     int64            `json:"response_bytes"`
	DistinctTokens    int64            `json:"distinct_tokens"`
	DistinctClientIPs int64            `json:"distinct_client_ips"`
	FirstRequestAt    *time.Time       `json:"first_request_at"`
	LastRequestAt     *time.Time       `json:"last_request_at"`
	ByStatusClass     map[string]int64 `json:"by_status_class"`
}

// Summary returns totals since a point in time.
func (s *Store) Summary(ctx context.Context, since time.Time) (*Summary, error) {
	out := &Summary{ByStatusClass: map[string]int64{}}
	var avg sql.NullFloat64
	var maxD, reqB, respB, ce, se, uf, rl, bl sql.NullInt64
	var first, last sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),
			SUM(status BETWEEN 400 AND 499), SUM(status >= 500), SUM(error IS NOT NULL AND error <> ?),
			SUM(error = ?),
			SUM(status IN (403, 429) AND rate_limit_remaining = 0),
			AVG(duration_ms), MAX(duration_ms), SUM(request_bytes), SUM(response_bytes),
			COUNT(DISTINCT token_hash), COUNT(DISTINCT client_ip), MIN(requested_at), MAX(requested_at)
		FROM github_proxy_requests WHERE requested_at >= ?`, BlockedError, BlockedError, since).Scan(
		&out.Requests, &ce, &se, &uf, &bl, &rl, &avg, &maxD, &reqB, &respB,
		&out.DistinctTokens, &out.DistinctClientIPs, &first, &last)
	if err != nil {
		return nil, err
	}
	out.ClientErrors, out.ServerErrors, out.UpstreamFailures, out.RateLimited = ce.Int64, se.Int64, uf.Int64, rl.Int64
	out.Blocked = bl.Int64
	out.AvgDurationMs, out.MaxDurationMs, out.RequestBytes, out.ResponseBytes = round1(avg.Float64), maxD.Int64, reqB.Int64, respB.Int64
	if first.Valid {
		out.FirstRequestAt = &first.Time
	}
	if last.Valid {
		out.LastRequestAt = &last.Time
	}

	rows, err := s.db.QueryContext(ctx, `SELECT CONCAT(FLOOR(status / 100), 'xx') AS cls, COUNT(*)
		FROM github_proxy_requests WHERE requested_at >= ? GROUP BY cls`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cls string
		var n int64
		if err := rows.Scan(&cls, &n); err != nil {
			return nil, err
		}
		out.ByStatusClass[cls] = n
	}
	return out, rows.Err()
}

// CallerStats aggregates one caller.
type CallerStats struct {
	Caller        string    `json:"caller"`
	TokenKinds    []string  `json:"token_kinds"`
	Requests      int64     `json:"requests"`
	Errors        int64     `json:"errors"`
	AvgDurationMs float64   `json:"avg_duration_ms"`
	FirstSeenAt   time.Time `json:"first_seen_at"`
	LastSeenAt    time.Time `json:"last_seen_at"`
	Tokens        int64     `json:"distinct_tokens"`
	ClientIPs     []string  `json:"client_ips"`
	UserAgents    []string  `json:"user_agents"`
}

// Callers returns per-caller totals, busiest first.
func (s *Store) Callers(ctx context.Context, since time.Time, limit int) ([]CallerStats, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+callerExpr+` AS caller,
			GROUP_CONCAT(DISTINCT r.token_kind ORDER BY r.token_kind SEPARATOR '\n'),
			COUNT(*), SUM(r.status >= 400), AVG(r.duration_ms), MIN(r.requested_at), MAX(r.requested_at),
			COUNT(DISTINCT r.token_hash),
			SUBSTRING_INDEX(GROUP_CONCAT(DISTINCT r.client_ip ORDER BY r.client_ip SEPARATOR '\n'), '\n', 10),
			SUBSTRING_INDEX(GROUP_CONCAT(DISTINCT r.user_agent ORDER BY r.user_agent SEPARATOR '\n'), '\n', 10)
		`+fromJoin+` WHERE r.requested_at >= ? GROUP BY caller ORDER BY COUNT(*) DESC LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CallerStats{}
	for rows.Next() {
		var c CallerStats
		var kinds, ips, uas sql.NullString
		var errs sql.NullInt64
		var avg sql.NullFloat64
		if err := rows.Scan(&c.Caller, &kinds, &c.Requests, &errs, &avg, &c.FirstSeenAt, &c.LastSeenAt, &c.Tokens, &ips, &uas); err != nil {
			return nil, err
		}
		c.Errors, c.AvgDurationMs = errs.Int64, round1(avg.Float64)
		c.TokenKinds, c.ClientIPs, c.UserAgents = lines(kinds), lines(ips), lines(uas)
		out = append(out, c)
	}
	return out, rows.Err()
}

// RouteStats aggregates one normalised endpoint.
type RouteStats struct {
	Method        string  `json:"method"`
	Route         string  `json:"route"`
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
	MaxDurationMs int64   `json:"max_duration_ms"`
	Callers       int64   `json:"distinct_callers"`
}

// Routes returns the busiest endpoints.
func (s *Store) Routes(ctx context.Context, since time.Time, caller string, limit int) ([]RouteStats, error) {
	where, args := "r.requested_at >= ?", []any{since}
	if caller != "" {
		where += " AND " + callerExpr + " = ?"
		args = append(args, caller)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT r.method, r.route, COUNT(*), SUM(r.status >= 400),
			AVG(r.duration_ms), MAX(r.duration_ms), COUNT(DISTINCT `+callerExpr+`)
		`+fromJoin+` WHERE `+where+` GROUP BY r.method, r.route ORDER BY COUNT(*) DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RouteStats{}
	for rows.Next() {
		var r RouteStats
		var errs sql.NullInt64
		var avg sql.NullFloat64
		if err := rows.Scan(&r.Method, &r.Route, &r.Requests, &errs, &avg, &r.MaxDurationMs, &r.Callers); err != nil {
			return nil, err
		}
		r.Errors, r.AvgDurationMs = errs.Int64, round1(avg.Float64)
		out = append(out, r)
	}
	return out, rows.Err()
}

// TimelinePoint is one bucket.
type TimelinePoint struct {
	Bucket        string  `json:"bucket"`
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

var bucketFormats = map[string]string{
	"minute": "%Y-%m-%dT%H:%i:00Z",
	"hour":   "%Y-%m-%dT%H:00:00Z",
	"day":    "%Y-%m-%dT00:00:00Z",
}

// Timeline returns request counts per bucket (minute, hour or day).
func (s *Store) Timeline(ctx context.Context, since time.Time, bucket, caller string) ([]TimelinePoint, error) {
	format, ok := bucketFormats[bucket]
	if !ok {
		format = bucketFormats["hour"]
	}
	where, args := "r.requested_at >= ?", []any{format, since}
	if caller != "" {
		where += " AND " + callerExpr + " = ?"
		args = append(args, caller)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DATE_FORMAT(r.requested_at, ?) AS b, COUNT(*),
			SUM(r.status >= 400), AVG(r.duration_ms)
		`+fromJoin+` WHERE `+where+` GROUP BY b ORDER BY b`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TimelinePoint{}
	for rows.Next() {
		var p TimelinePoint
		var errs sql.NullInt64
		var avg sql.NullFloat64
		if err := rows.Scan(&p.Bucket, &p.Requests, &errs, &avg); err != nil {
			return nil, err
		}
		p.Errors, p.AvgDurationMs = errs.Int64, round1(avg.Float64)
		out = append(out, p)
	}
	return out, rows.Err()
}

// RequestRow is one logged request.
type RequestRow struct {
	ID                 int64      `json:"id"`
	RequestedAt        time.Time  `json:"requested_at"`
	DurationMs         int64      `json:"duration_ms"`
	Method             string     `json:"method"`
	Path               string     `json:"path"`
	Query              *string    `json:"query"`
	Route              string     `json:"route"`
	GraphQLOperation   *string    `json:"graphql_operation"`
	Status             int        `json:"status"`
	ResponseBytes      int64      `json:"response_bytes"`
	Caller             string     `json:"caller"`
	TokenKind          string     `json:"token_kind"`
	TokenHash          *string    `json:"token_fingerprint"`
	ClientIP           string     `json:"client_ip"`
	UserAgent          *string    `json:"user_agent"`
	RateLimitResource  *string    `json:"rate_limit_resource"`
	RateLimitLimit     *int64     `json:"rate_limit_limit"`
	RateLimitRemaining *int64     `json:"rate_limit_remaining"`
	RateLimitReset     *time.Time `json:"rate_limit_reset"`
	GitHubRequestID    *string    `json:"github_request_id"`
	Error              *string    `json:"error"`
}

// RequestFilter narrows the request list.
type RequestFilter struct {
	Since    time.Time
	Caller   string
	Method   string
	Status   int // exact status, or 4/5 for a class
	Route    string
	BeforeID int64
	Limit    int
}

// Requests lists the most recent requests, newest first (keyset pagination via BeforeID).
func (s *Store) Requests(ctx context.Context, f RequestFilter) ([]RequestRow, error) {
	conds, args := []string{"r.requested_at >= ?"}, []any{f.Since}
	if f.Caller != "" {
		conds, args = append(conds, callerExpr+" = ?"), append(args, f.Caller)
	}
	if f.Method != "" {
		conds, args = append(conds, "r.method = ?"), append(args, strings.ToUpper(f.Method))
	}
	if f.Status >= 100 {
		conds, args = append(conds, "r.status = ?"), append(args, f.Status)
	} else if f.Status >= 1 && f.Status <= 5 {
		conds, args = append(conds, "r.status BETWEEN ? AND ?"), append(args, f.Status*100, f.Status*100+99)
	}
	if f.Route != "" {
		conds, args = append(conds, "r.route = ?"), append(args, f.Route)
	}
	if f.BeforeID > 0 {
		conds, args = append(conds, "r.id < ?"), append(args, f.BeforeID)
	}
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, `SELECT r.id, r.requested_at, r.duration_ms, r.method, r.path, r.query, r.route,
			r.graphql_operation, r.status, r.response_bytes, `+callerExpr+`, r.token_kind, LEFT(r.token_hash, 12),
			r.client_ip, r.user_agent, r.rate_limit_resource, r.rate_limit_limit, r.rate_limit_remaining,
			r.rate_limit_reset, r.github_request_id, r.error
		`+fromJoin+` WHERE `+strings.Join(conds, " AND ")+` ORDER BY r.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RequestRow{}
	for rows.Next() {
		var r RequestRow
		var q, gql, th, ua, rlr, ghid, e sql.NullString
		var lim, rem sql.NullInt64
		var reset sql.NullTime
		if err := rows.Scan(&r.ID, &r.RequestedAt, &r.DurationMs, &r.Method, &r.Path, &q, &r.Route, &gql, &r.Status,
			&r.ResponseBytes, &r.Caller, &r.TokenKind, &th, &r.ClientIP, &ua, &rlr, &lim, &rem, &reset, &ghid, &e); err != nil {
			return nil, err
		}
		r.Query, r.GraphQLOperation, r.TokenHash, r.UserAgent = ptrS(q), ptrS(gql), ptrS(th), ptrS(ua)
		r.RateLimitResource, r.GitHubRequestID, r.Error = ptrS(rlr), ptrS(ghid), ptrS(e)
		r.RateLimitLimit, r.RateLimitRemaining = ptrI(lim), ptrI(rem)
		if reset.Valid {
			r.RateLimitReset = &reset.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RateLimit is the latest quota seen for a credential and resource.
type RateLimit struct {
	Caller      string    `json:"caller"`
	TokenKind   string    `json:"token_kind"`
	Fingerprint *string   `json:"token_fingerprint"`
	ClientIP    string    `json:"client_ip"`
	Resource    string    `json:"resource"`
	Limit       int64     `json:"limit"`
	Remaining   int64     `json:"remaining"`
	Used        *int64    `json:"used"`
	UsedPercent float64   `json:"used_percent"`
	ResetAt     time.Time `json:"reset_at"`
	SeenAt      time.Time `json:"seen_at"`
}

// RateLimits returns, per credential and resource, the last rate-limit
// headers GitHub sent, most exhausted first. Windows that already reset are skipped.
func (s *Store) RateLimits(ctx context.Context, since time.Time) ([]RateLimit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+callerExpr+`, r.token_kind, LEFT(r.token_hash, 12), r.client_ip,
			r.rate_limit_resource, r.rate_limit_limit, r.rate_limit_remaining, r.rate_limit_used,
			r.rate_limit_reset, r.requested_at
		FROM github_proxy_requests r
		JOIN (SELECT MAX(id) AS id FROM github_proxy_requests
		      WHERE requested_at >= ? AND rate_limit_resource IS NOT NULL AND rate_limit_limit > 0
		      GROUP BY COALESCE(token_hash, client_ip), rate_limit_resource) latest ON latest.id = r.id
		LEFT JOIN github_proxy_tokens t ON t.token_hash = r.token_hash
		WHERE r.rate_limit_reset > UTC_TIMESTAMP()
		ORDER BY r.rate_limit_remaining / r.rate_limit_limit ASC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RateLimit{}
	for rows.Next() {
		var rl RateLimit
		var fp sql.NullString
		var used sql.NullInt64
		if err := rows.Scan(&rl.Caller, &rl.TokenKind, &fp, &rl.ClientIP, &rl.Resource, &rl.Limit, &rl.Remaining,
			&used, &rl.ResetAt, &rl.SeenAt); err != nil {
			return nil, err
		}
		rl.Fingerprint, rl.Used = ptrS(fp), ptrI(used)
		if rl.Limit > 0 {
			rl.UsedPercent = round1(100 * float64(rl.Limit-rl.Remaining) / float64(rl.Limit))
		}
		out = append(out, rl)
	}
	return out, rows.Err()
}

func lines(s sql.NullString) []string {
	if !s.Valid || s.String == "" {
		return []string{}
	}
	return strings.Split(s.String, "\n")
}

func ptrS(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func ptrI(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}
