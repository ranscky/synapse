package api

import (
	"sync"
	"time"
)

// bucket represents a token bucket for a specific client
type bucket struct {
	tokens     int64     // current token count
	lastUpdate time.Time // last time tokens were added
}

// RateLimiter implements a token bucket rate limiter
type RateLimiter struct {
	rate      int64         // tokens per interval
	interval  time.Duration // time interval
	buckets   map[string]*bucket
	mutex     sync.Mutex
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(rate int64, interval time.Duration) *RateLimiter {
	return &RateLimiter{
		rate:     rate,
		interval: interval,
		buckets:  make(map[string]*bucket),
	}
}

// Allow checks if a request is allowed and consumes a token if so
func (rl *RateLimiter) Allow(clientID string) bool {
	rl.mutex.Lock()
	defer rl.mutex.Unlock()

	now := time.Now()
	b, exists := rl.buckets[clientID]
	if !exists {
		// Create new bucket with full tokens
		rl.buckets[clientID] = &bucket{
			tokens:     rl.rate - 1, // consume one token
			lastUpdate: now,
		}
		return true
	}

	// Add tokens based on elapsed time
	elapsed := now.Sub(b.lastUpdate)
	tokensToAdd := int64(elapsed / rl.interval) * rl.rate
	if tokensToAdd > 0 {
		b.tokens += tokensToAdd
		if b.tokens > rl.rate {
			b.tokens = rl.rate
		}
		b.lastUpdate = now
	}

	// Check if we have tokens available
	if b.tokens > 0 {
		b.tokens--
		return true
	}

	return false
}
