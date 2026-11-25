package grimoire

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// SentinelKeys are the keys Grimoire bumps when configuration is published: the
// application's own and the one for inherited globals (guibranco/grimoire-api#95).
var SentinelKeys = []string{"Sentinel", "Global:Sentinel"}

// maxBackoff caps the wait between checks while Grimoire is failing.
const maxBackoff = 5 * time.Minute

// Watcher follows Grimoire's sentinel keys and calls onChange when one of them
// changes, so values are re-read only after a publish instead of on a timer.
// Each check is a conditional request per key: an unchanged key costs a 304.
//
// It follows the shared client spec (docs/client-library/refresh-spec.md in
// grimoire-api): the baseline is recorded before the values are read, a key
// appearing or disappearing counts as a change, and the new state is kept
// only once onChange succeeded, so a failed reload is retried on the next check.
type Watcher struct {
	client   *Client
	keys     []string
	interval time.Duration
	onChange func(context.Context) error
	log      *slog.Logger

	mu    sync.Mutex
	state map[string]KeyState
	// reloadOnFirstCheck is set when the baseline couldn't be read at start-up:
	// the first successful check then reloads once, so a publish made around
	// start-up is never missed.
	reloadOnFirstCheck bool
}

// NewWatcher watches keys (SentinelKeys when empty) every interval.
func NewWatcher(client *Client, keys []string, interval time.Duration, onChange func(context.Context) error, log *slog.Logger) *Watcher {
	if len(keys) == 0 {
		keys = SentinelKeys
	}
	return &Watcher{client: client, keys: keys, interval: interval, onChange: onChange, log: log, state: map[string]KeyState{}}
}

// Prime records the current state of the watched keys without calling
// onChange. Call it before reading the values the watcher protects. On error
// the first successful Check reloads once.
func (w *Watcher) Prime(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	states := make(map[string]KeyState, len(w.keys))
	for _, key := range w.keys {
		s, _, err := w.client.ConfigurationIfChanged(ctx, key, nil)
		if err != nil {
			w.reloadOnFirstCheck = true
			return err
		}
		states[key] = s
	}
	w.state = states
	return nil
}

// Check reads the watched keys now and calls onChange when one changed.
// It reports whether onChange ran successfully.
func (w *Watcher) Check(ctx context.Context) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	changed := w.reloadOnFirstCheck
	next := make(map[string]KeyState, len(w.keys))
	for _, key := range w.keys {
		prev, known := w.state[key]
		var prevPtr *KeyState
		if known {
			prevPtr = &prev
		}
		s, notModified, err := w.client.ConfigurationIfChanged(ctx, key, prevPtr)
		if err != nil {
			return false, err
		}
		next[key] = s
		if notModified || !known {
			continue
		}
		if s.Exists != prev.Exists || s.Value != prev.Value {
			changed = true
		}
	}

	if changed {
		if err := w.onChange(ctx); err != nil {
			return false, err // keep the old state: the change is seen again next time
		}
	}
	w.state = next
	w.reloadOnFirstCheck = false
	return changed, nil
}

// Run checks every interval (plus up to 20 % jitter) until ctx is done,
// backing off exponentially (up to 5 minutes) while checks fail.
func (w *Watcher) Run(ctx context.Context) {
	failures := 0
	for {
		t := time.NewTimer(w.nextDelay(failures))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		changed, err := w.Check(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			failures++
			w.log.Warn("Could not check Grimoire for published changes, keeping the current settings",
				"category", "grimoire", "error", err, "failures", failures)
		case err == nil:
			if failures > 0 {
				w.log.Info("Grimoire reachable again", "category", "grimoire")
			}
			failures = 0
			if changed {
				w.log.Info("Grimoire published changes, settings reloaded", "category", "grimoire")
			}
		}
	}
}

func (w *Watcher) nextDelay(failures int) time.Duration {
	if failures > 0 {
		d := w.interval << min(failures, 16)
		if d <= 0 || d > maxBackoff {
			return maxBackoff
		}
		return d
	}
	return w.interval + time.Duration(rand.Int64N(int64(w.interval)/5+1))
}
