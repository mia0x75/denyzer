package resolver

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/util"

	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

// ErrRateLimited signals that handleReq must write no response.
var ErrRateLimited = errors.New("client rate-limited")

const (
	// rateLimitBucketCap bounds the in-memory bucket store; hardcoded for v1.
	rateLimitBucketCap = 16384

	// rateLimitJanitorInterval is how often idle buckets are reclaimed.
	rateLimitJanitorInterval = time.Minute
)

type RateLimitingResolver struct {
	configurable[*config.RateLimit]
	NextResolver
	typed

	store  *bucketStore
	clock  func() time.Time
	logger *logrus.Entry
}

func NewRateLimitingResolver(ctx context.Context, cfg config.RateLimit) *RateLimitingResolver {
	r := &RateLimitingResolver{
		configurable: withConfig(&cfg),
		typed:        withType("rate-limiting"),
		clock:        time.Now,
		logger:       log.PrefixedLog("rate-limiting"),
	}
	if !cfg.IsEnabled() {
		return r
	}
	r.store = newBucketStore(rate.Limit(cfg.Rate), int(cfg.Burst), rateLimitBucketCap)
	r.store.startJanitor(ctx, rateLimitJanitorInterval)

	return r
}

func (r *RateLimitingResolver) Resolve(ctx context.Context, req *model.Request) (*model.Response, error) {
	if !r.IsEnabled() {
		return r.next.Resolve(ctx, req)
	}
	ip := req.ClientIP
	if ip == nil || r.isAllowlisted(ip) {
		return r.next.Resolve(ctx, req)
	}
	key := bucketKey(ip, r.cfg.IPv4Prefix, r.cfg.IPv6Prefix)
	entry, allowed := r.store.allowAt(key, r.clock())
	if allowed {
		return r.next.Resolve(ctx, req)
	}
	// entry is nil when the bucket store is full; the query is still dropped.
	r.recordDrop(req, entry)

	return nil, ErrRateLimited
}

func (r *RateLimitingResolver) isAllowlisted(ip net.IP) bool {
	for _, n := range r.cfg.ParsedAllowlist() {
		if n.Contains(ip) {
			return true
		}
	}

	return false
}

func (r *RateLimitingResolver) recordDrop(req *model.Request, e *bucketEntry) {
	if e == nil {
		return
	}
	now := r.clock().UnixNano()
	prev := e.lastLogged.Load()
	if now-prev < int64(time.Second) || !e.lastLogged.CompareAndSwap(prev, now) {
		return
	}
	fields := logrus.Fields{
		"client_ip":      req.ClientIP,
		logFieldProtocol: req.Protocol,
		"qname":          util.QuestionToString(req.Req.Question),
		"bucket_tokens":  e.limiter.Tokens(),
	}
	if len(req.Req.Question) > 0 {
		fields["qtype"] = req.Req.Question[0].Qtype
	}
	r.logger.WithFields(fields).Warn("dropped query")
}
