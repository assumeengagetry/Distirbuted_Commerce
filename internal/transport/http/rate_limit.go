package httptransport

import (
	"container/list"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"github.com/assumeengagetry/distributed-commerce/internal/auth"
)

type RateLimitConfig struct {
	RequestsPerSecond float64
	Burst             int
	EntryTTL          time.Duration
	MaxEntries        int
}

type rateLimitVisitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
	element  *list.Element
}

type ipRateLimiter struct {
	mu         sync.Mutex
	visitors   map[string]*rateLimitVisitor
	recency    *list.List
	overflow   *rate.Limiter
	limit      rate.Limit
	burst      int
	entryTTL   time.Duration
	maxEntries int
	now        func() time.Time
}

func newIPRateLimiter(cfg RateLimitConfig, now func() time.Time) (*ipRateLimiter, error) {
	if cfg.RequestsPerSecond <= 0 || cfg.Burst <= 0 || cfg.EntryTTL <= 0 || cfg.MaxEntries <= 0 {
		return nil, fmt.Errorf("rate limit configuration must be positive")
	}
	if now == nil {
		return nil, fmt.Errorf("rate limiter clock is required")
	}
	return &ipRateLimiter{
		visitors:   make(map[string]*rateLimitVisitor),
		recency:    list.New(),
		overflow:   rate.NewLimiter(rate.Limit(cfg.RequestsPerSecond), cfg.Burst),
		limit:      rate.Limit(cfg.RequestsPerSecond),
		burst:      cfg.Burst,
		entryTTL:   cfg.EntryTTL,
		maxEntries: cfg.MaxEntries,
		now:        now,
	}, nil
}

func (l *ipRateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	visitor, ok := l.visitors[key]
	if !ok {
		l.removeExpired(now)
		if len(l.visitors) >= l.maxEntries {
			if !l.overflow.AllowN(now, 1) {
				return false
			}
			l.removeOldest()
		}
		visitor = &rateLimitVisitor{limiter: rate.NewLimiter(l.limit, l.burst)}
		visitor.element = l.recency.PushFront(key)
		l.visitors[key] = visitor
	} else {
		l.recency.MoveToFront(visitor.element)
	}
	visitor.lastSeen = now
	return visitor.limiter.AllowN(now, 1)
}

func (l *ipRateLimiter) removeExpired(now time.Time) {
	for {
		oldest := l.recency.Back()
		if oldest == nil {
			return
		}
		key := oldest.Value.(string)
		visitor := l.visitors[key]
		if now.Sub(visitor.lastSeen) <= l.entryTTL {
			return
		}
		delete(l.visitors, key)
		l.recency.Remove(oldest)
	}
}

func (l *ipRateLimiter) removeOldest() {
	oldest := l.recency.Back()
	if oldest == nil {
		return
	}
	delete(l.visitors, oldest.Value.(string))
	l.recency.Remove(oldest)
}

func rateLimitMiddleware(limiter *ipRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !limiter.allow(remoteIP(c.Request.RemoteAddr)) {
			writeRateLimitError(c, limiter)
			writeAPIError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
			return
		}
		c.Next()
	}
}

func principalRateLimitMiddleware(limiter *ipRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := auth.PrincipalFromContext(c.Request.Context())
		if !ok {
			writeAPIError(c, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		if !limiter.allow("user:" + principal.UserID.String()) {
			writeRateLimitError(c, limiter)
			writeAPIError(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests")
			return
		}
		c.Next()
	}
}

func writeRateLimitError(c *gin.Context, limiter *ipRateLimiter) {
	retryAfter := int(math.Ceil(1 / float64(limiter.limit)))
	if retryAfter < 1 {
		retryAfter = 1
	}
	c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
}

func remoteIP(remoteAddress string) string {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return remoteAddress
	}
	if zone := strings.LastIndexByte(host, '%'); zone >= 0 {
		host = host[:zone]
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String()
	}
	return host
}
