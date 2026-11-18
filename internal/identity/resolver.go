package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Identity is what a token resolves to.
type Identity struct {
	Caller      string // label used in dashboards: login, "installation:<owner>", "app:<id>"
	Login       string
	AccountType string // User, Bot, Organization
	Note        string
}

// Store persists resolved identities (implemented by the MySQL store).
type Store interface {
	// LookupIdentity returns the stored caller label for a token hash, or "" when unresolved.
	LookupIdentity(ctx context.Context, hash string) (string, error)
	SaveIdentity(ctx context.Context, hash, kind string, id Identity, resolveErr string) error
}

type cacheEntry struct {
	caller    string
	state     int // 0 pending, 1 resolved, 2 failed (retry after)
	retryAt   time.Time
	touchedAt time.Time
}

// Resolver resolves token fingerprints to accounts in the background, so the
// proxied request never waits for it. Each new token costs the caller one
// extra GitHub API call (GET /user, or GET /installation/repositories for
// installation tokens); app JWTs are labelled from their claims for free.
type Resolver struct {
	upstream  *url.URL
	userAgent string
	store     Store
	enabled   bool
	log       *slog.Logger
	http      *http.Client

	mu    sync.Mutex
	cache map[string]*cacheEntry
	jobs  chan Credential
}

// NewResolver creates a resolver. When enabled is false, tokens are still
// fingerprinted and counted but never looked up on GitHub.
func NewResolver(upstream *url.URL, userAgent string, store Store, enabled bool, log *slog.Logger) *Resolver {
	return &Resolver{
		upstream:  upstream,
		userAgent: userAgent,
		store:     store,
		enabled:   enabled,
		log:       log,
		http:      &http.Client{Timeout: 15 * time.Second},
		cache:     map[string]*cacheEntry{},
		jobs:      make(chan Credential, 256),
	}
}

// Start runs the worker goroutines until ctx is cancelled.
func (r *Resolver) Start(ctx context.Context, workers int) {
	for range workers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case c := <-r.jobs:
					r.resolve(ctx, c)
				}
			}
		}()
	}
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.prune(24 * time.Hour)
			}
		}
	}()
}

// Observe is called for every proxied request. It returns the caller label
// if already known (used for rate-limit warnings) and schedules resolution
// for tokens seen for the first time.
func (r *Resolver) Observe(c Credential) string {
	if c.Kind == KindNone {
		return "anonymous"
	}
	now := time.Now()
	r.mu.Lock()
	e, ok := r.cache[c.Hash]
	if ok {
		e.touchedAt = now
		caller := e.caller
		retry := e.state == 2 && now.After(e.retryAt)
		if retry {
			e.state = 0
		}
		r.mu.Unlock()
		if retry {
			r.enqueue(c)
		}
		if caller == "" {
			return "token:" + ShortHash(c.Hash)
		}
		return caller
	}
	r.cache[c.Hash] = &cacheEntry{touchedAt: now}
	r.mu.Unlock()
	r.enqueue(c)
	return "token:" + ShortHash(c.Hash)
}

func (r *Resolver) enqueue(c Credential) {
	select {
	case r.jobs <- c:
	default:
		r.fail(c.Hash, time.Minute) // busy: try again on a later request
	}
}

func (r *Resolver) set(hash, caller string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.cache[hash]; ok {
		e.caller, e.state = caller, 1
	}
}

func (r *Resolver) fail(hash string, after time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.cache[hash]; ok {
		e.state, e.retryAt = 2, time.Now().Add(after)
	}
}

func (r *Resolver) prune(maxIdle time.Duration) {
	cutoff := time.Now().Add(-maxIdle)
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, e := range r.cache {
		if e.touchedAt.Before(cutoff) {
			delete(r.cache, k)
		}
	}
}

func (r *Resolver) resolve(ctx context.Context, c Credential) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	if caller, err := r.store.LookupIdentity(ctx, c.Hash); err == nil && caller != "" {
		r.set(c.Hash, caller)
		return
	} else if err != nil {
		r.log.Warn("Identity lookup in MySQL failed", "category", "identity", "error", err)
		r.fail(c.Hash, time.Minute)
		return
	}

	id, resolveErr, transient := r.lookup(ctx, c)
	if transient {
		r.log.Warn("Could not resolve token identity, will retry", "category", "identity",
			"token_hash", ShortHash(c.Hash), "token_kind", c.Kind, "error", resolveErr)
		r.fail(c.Hash, 10*time.Minute)
		return
	}
	if err := r.store.SaveIdentity(ctx, c.Hash, c.Kind, id, resolveErr); err != nil {
		r.log.Warn("Saving token identity failed", "category", "identity", "error", err)
		r.fail(c.Hash, time.Minute)
		return
	}
	r.set(c.Hash, id.Caller)
	r.log.Info("New caller identified", "category", "identity",
		"caller", id.Caller, "token_kind", c.Kind, "token_hash", ShortHash(c.Hash))
}

// lookup returns the identity, a permanent error note (stored), or transient=true.
func (r *Resolver) lookup(ctx context.Context, c Credential) (id Identity, note string, transient bool) {
	short := ShortHash(c.Hash)
	switch c.Kind {
	case KindAppJWT:
		if app := AppIDFromJWT(c.Token); app != "" {
			return Identity{Caller: "app:" + app, Note: "GitHub App JWT"}, "", false
		}
		return Identity{Caller: "app:unknown"}, "JWT without iss claim", false
	}

	if !r.enabled {
		return Identity{Caller: "token:" + short}, "resolution disabled", false
	}

	if c.Kind == KindInstallation {
		var body struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				Owner struct {
					Login string `json:"login"`
					Type  string `json:"type"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		status, err := r.get(ctx, c.Token, "/installation/repositories?per_page=1", &body)
		if err != nil {
			return id, err.Error(), true
		}
		if status != http.StatusOK {
			return Identity{Caller: "invalid:" + short}, fmt.Sprintf("GitHub answered %d", status), false
		}
		if len(body.Repositories) == 0 {
			return Identity{Caller: "installation:" + short, Note: "installation with no repositories"}, "", false
		}
		o := body.Repositories[0].Owner
		return Identity{
			Caller: "installation:" + o.Login, Login: o.Login, AccountType: o.Type,
			Note: fmt.Sprintf("%d repositories", body.TotalCount),
		}, "", false
	}

	var user struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	status, err := r.get(ctx, c.Token, "/user", &user)
	if err != nil {
		return id, err.Error(), true
	}
	if status != http.StatusOK || user.Login == "" {
		return Identity{Caller: "invalid:" + short}, fmt.Sprintf("GitHub answered %d", status), false
	}
	return Identity{Caller: user.Login, Login: user.Login, AccountType: user.Type}, "", false
}

func (r *Resolver) get(ctx context.Context, token, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.upstream.String()+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", r.userAgent)
	resp, err := r.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return 0, fmt.Errorf("GitHub answered %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return 0, errors.New("invalid JSON from GitHub: " + err.Error())
	}
	return resp.StatusCode, nil
}
