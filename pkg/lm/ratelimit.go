package lm

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rateLimiter tracks LogicMonitor's X-Rate-Limit-Remaining and
// X-Rate-Limit-Window headers. Each request reserves one of the remaining
// requests; once none are left, new requests wait until the window resets.
type rateLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	known     bool
	remaining int
	resetAt   time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{now: time.Now}
}

// reserve takes one request from the current window and returns how long
// the caller must wait before sending it.
func (r *rateLimiter) reserve() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if !r.known || !now.Before(r.resetAt) {
		r.known = false
		return 0
	}
	if r.remaining > 0 {
		r.remaining--
		return 0
	}
	return r.resetAt.Sub(now)
}

// observe records the rate-limit headers of a LogicMonitor response. Within
// a window the remaining count only decreases, so responses to earlier
// requests cannot raise it.
func (r *rateLimiter) observe(header http.Header) {
	remaining, window, ok := rateLimitHeaders(header)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.known && now.Before(r.resetAt) {
		r.remaining = min(r.remaining, remaining)
		return
	}
	r.known = true
	r.remaining = remaining
	r.resetAt = now.Add(window)
}

// block marks the window as exhausted for d, as indicated by Retry-After.
func (r *rateLimiter) block(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.known = true
	r.remaining = 0
	r.resetAt = r.now().Add(d)
}

func rateLimitHeaders(header http.Header) (int, time.Duration, bool) {
	remaining, err := strconv.Atoi(strings.TrimSpace(header.Get("X-Rate-Limit-Remaining")))
	if err != nil || remaining < 0 {
		return 0, 0, false
	}
	windowSeconds, err := strconv.ParseFloat(strings.TrimSpace(header.Get("X-Rate-Limit-Window")), 64)
	if err != nil || math.IsNaN(windowSeconds) || math.IsInf(windowSeconds, 0) || windowSeconds <= 0 {
		return 0, 0, false
	}
	window := time.Duration(windowSeconds * float64(time.Second))
	if window <= 0 {
		return 0, 0, false
	}
	return remaining, window, true
}

// rateLimitRetryDelay returns a delay only when LogicMonitor's headers
// indicate that the rate-limit window will elapse within maxWait.
func rateLimitRetryDelay(resp *http.Response, maxWait time.Duration) (time.Duration, bool) {
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	remaining, window, ok := rateLimitHeaders(resp.Header)
	if !ok || remaining != 0 || window > maxWait {
		return 0, false
	}
	return window, true
}

// waitForRateLimit delays a new request while the shared rate-limit state
// shows no remaining requests, for at most the configured maximum wait.
func (c *Client) waitForRateLimit(ctx context.Context) error {
	maxWait := c.cfg.maxRateLimitWait()
	wait := min(c.shared.limiter.reserve(), maxWait)
	if wait <= 0 {
		return nil
	}
	// Stagger concurrent waiters slightly without exceeding maxWait.
	delay := min(wait+rand.N(50*time.Millisecond), maxWait)
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
		return context.DeadlineExceeded
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
