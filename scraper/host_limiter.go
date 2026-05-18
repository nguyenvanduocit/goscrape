package scraper

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/net/publicsuffix"
	"golang.org/x/time/rate"
)

type hostLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	rps      rate.Limit
	burst    int
}

func newHostLimiter(rps rate.Limit, burst int) *hostLimiter {
	return &hostLimiter{limiters: map[string]*rate.Limiter{}, rps: rps, burst: burst}
}

func hostKey(host string) string {
	if host == "" {
		return ""
	}
	if etld1, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return etld1
	}
	return host
}

func (h *hostLimiter) Wait(ctx context.Context, host string) error {
	if h == nil {
		return nil
	}
	key := hostKey(host)
	h.mu.Lock()
	lim, ok := h.limiters[key]
	if !ok {
		lim = rate.NewLimiter(h.rps, h.burst)
		h.limiters[key] = lim
	}
	h.mu.Unlock()
	if err := lim.Wait(ctx); err != nil {
		return fmt.Errorf("host rate limiter wait: %w", err)
	}
	return nil
}
