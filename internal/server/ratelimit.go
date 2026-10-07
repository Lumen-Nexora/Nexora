package server

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/api"
	"github.com/Lumen-Nexora/Nexora/internal/tenant"
	"golang.org/x/time/rate"
)

const maxIdleTime = 5 * time.Minute

type visitorEntry struct {
	rateLimiter *rate.Limiter
	lastSeen    time.Time
}

type rateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitorEntry
	rate     rate.Limit
	burst    int
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	rl := &rateLimiter{
		visitors: make(map[string]*visitorEntry),
		rate:     rate.Limit(rps),
		burst:    burst,
	}
	go rl.cleanup()
	return rl
}

func (rl *rateLimiter) getLimiter(key string) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, exists := rl.visitors[key]
	if !exists {
		entry = &visitorEntry{
			rateLimiter: rate.NewLimiter(rl.rate, rl.burst),
			lastSeen:    time.Now(),
		}
		rl.visitors[key] = entry
	} else {
		entry.lastSeen = time.Now()
	}
	return entry.rateLimiter
}

func (rl *rateLimiter) cleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		for key, entry := range rl.visitors {
			if now.Sub(entry.lastSeen) > maxIdleTime {
				delete(rl.visitors, key)
			}
		}
		rl.mu.Unlock()
	}
}

func extractClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		ip := strings.TrimSpace(xrip)
		if ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimit returns a middleware that limits requests per-tenant (or per-IP for unauthenticated).
func RateLimit(rps float64, burst int) func(http.Handler) http.Handler {
	globalLimiter := newRateLimiter(rps, burst)
	tenantBurst := burst * 10
	tenantLimiters := newRateLimiter(rps*10, tenantBurst) // 10x for authenticated tenants

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check global rate first
			gLim := globalLimiter.getLimiter("global")
			gRes := gLim.Reserve()
			if !gRes.OK() || gRes.Delay() > 0 {
				gRes.Cancel()
				w.Header().Set("Retry-After", "1")
				w.Header().Set("RateLimit-Limit", strconv.Itoa(burst))
				w.Header().Set("RateLimit-Remaining", "0")
				w.Header().Set("RateLimit-Reset", "1")
				api.Error(w, http.StatusTooManyRequests, "RATE_LIMITED", "global rate limit exceeded")
				return
			}

			// Per-tenant or per-IP limiting
			tid := tenant.IDFromContext(r.Context())
			key := tid
			effectiveBurst := tenantBurst
			if key == "" {
				key = extractClientIP(r)
				effectiveBurst = burst
			}

			tLim := tenantLimiters.getLimiter(key)
			tRes := tLim.Reserve()
			if !tRes.OK() || tRes.Delay() > 0 {
				tRes.Cancel()
				delaySec := 1
				if tRes.Delay() > 0 {
					delaySec = int(tRes.Delay().Seconds()) + 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(delaySec))
				w.Header().Set("RateLimit-Limit", strconv.Itoa(effectiveBurst))
				w.Header().Set("RateLimit-Remaining", "0")
				w.Header().Set("RateLimit-Reset", strconv.Itoa(delaySec))
				api.Error(w, http.StatusTooManyRequests, "RATE_LIMITED", "rate limit exceeded")
				return
			}

			remaining := int(tLim.Tokens())
			if remaining < 0 {
				remaining = 0
			}
			w.Header().Set("RateLimit-Limit", strconv.Itoa(effectiveBurst))
			w.Header().Set("RateLimit-Remaining", strconv.Itoa(remaining))
			w.Header().Set("RateLimit-Reset", "1")

			next.ServeHTTP(w, r)
		})
	}
}
