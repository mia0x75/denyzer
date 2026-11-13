package resolver

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mia0x75/denyzer/log"

	"github.com/mia0x75/denyzer/cache"
	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/evt"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/util"
	"github.com/miekg/dns"
)

const (
	defaultCachingCleanUpInterval = 5 * time.Second

	// cachedReason is the response reason used for cache hits.
	cachedReason = "CACHED"
)

// CachingResolver caches answers from dns queries with their TTL time,
// to avoid external resolver calls for recurrent queries
type CachingResolver struct {
	configurable[*config.Caching]
	NextResolver
	typed

	// prefetchDO makes prefetch reloads request DNSSEC records (DO bit) so a
	// reload can't replace a signed entry with an unsigned one. Set from
	// dnssec.validate — unsigned caches shouldn't grow with RRSIGs.
	prefetchDO bool

	resultCache cache.ExpiringCache[[]byte]

	compiledExclusions []*regexp.Regexp
}

// NewCachingResolver creates a new resolver instance
func NewCachingResolver(ctx context.Context, cfg config.Caching, dnssecCfg config.DNSSEC) (*CachingResolver, error) {
	return newCachingResolver(ctx, cfg, dnssecCfg)
}

func newCachingResolver(ctx context.Context,
	cfg config.Caching,
	dnssecCfg config.DNSSEC,
) (*CachingResolver, error) {
	c := &CachingResolver{
		configurable: withConfig(&cfg),
		typed:        withType("caching"),
		prefetchDO:   dnssecCfg.Validate,
	}

	configureCaches(ctx, c, &cfg)
	err := configureExclusions(c, &cfg)

	return c, err
}

// reloadCacheEntry reloads a prefetched entry by re-resolving it upstream.
func (r *CachingResolver) reloadCacheEntry(ctx context.Context, cacheKey string) (*[]byte, time.Duration) {
	qType, domainName := util.ExtractCacheKey(cacheKey)
	ctx, logger := r.log(ctx)

	if logger.Enabled(ctx, slog.LevelDebug) {
		logger.DebugContext(ctx, fmt.Sprintf("prefetching '%s' (%s)", util.Obfuscate(domainName), qType))
	}

	req := newRequest(dns.Fqdn(domainName), qType)

	// Prefetch reloads bypass the resolvers above the cache, including the DNSSEC
	// resolver that sets DO on normal queries. When validation is enabled, request
	// DNSSEC records so a reload can't replace a signed entry with an unsigned one
	// (which would then fail re-validation as bogus on the next hit).
	if r.prefetchDO {
		req.Req.SetEdns0(ednsUDPSize, true)
	}

	response, err := r.next.Resolve(ctx, req)
	if err != nil {
		logger.ErrorContext(ctx, fmt.Sprintf("can't prefetch '%s' %v", domainName, err))

		return nil, 0
	}

	// only refresh entries the normal put path would cache: upstream-derived,
	// successful, not truncated and without the CD flag (mirrors putInCache).
	if !isCacheableResponseType(response.RType) ||
		response.Res.Rcode != dns.RcodeSuccess || !isResponseCacheable(response.Res) {
		return nil, 0
	}

	// clamp the record TTLs before packing so the stored bytes carry the same
	// TTLs the normal put path produces.
	ttl := r.adjustTTLs(response.Res.Answer)

	packed, err := packForCache(ctx, logger, response.Res)
	if err != nil {
		return nil, 0
	}

	return &packed, ttl
}

func configureCaches(ctx context.Context, c *CachingResolver, cfg *config.Caching) {
	options := cache.Options{
		CleanupInterval: defaultCachingCleanUpInterval,
		MaxSize:         uint(max(cfg.MaxItemsCount, 0)),
		OnAfterPutFn: func(newSize int) {
			evt.Bus().Publish(evt.CachingResultCacheChanged, newSize)
		},
	}

	if !cfg.Prefetching {
		resultCache := cache.NewCache[[]byte](options)
		resultCache.Start(ctx)
		c.resultCache = resultCache

		return
	}

	c.resultCache = cache.NewPrefetchingCache[[]byte](ctx, options, cache.PrefetchingOptions[[]byte]{
		ReloadFn:          c.reloadCacheEntry,
		PrefetchExpires:   time.Duration(cfg.PrefetchExpires),
		PrefetchThreshold: cfg.PrefetchThreshold,
		MaxItemsCount:     cfg.PrefetchMaxItemsCount,
		OnPrefetchEntryReloaded: func(key string) {
			evt.Bus().Publish(evt.CachingDomainPrefetched, key)
		},
		OnPrefetchCacheHit: func(key string) {
			evt.Bus().Publish(evt.CachingPrefetchCacheHit, key)
		},
	})
}

func configureExclusions(c *CachingResolver, cfg *config.Caching) error {
	compiled := []*regexp.Regexp{}
	for _, expStr := range cfg.Exclude {
		if !strings.HasPrefix(expStr, "/") || !strings.HasSuffix(expStr, "/") {
			return fmt.Errorf("cache exclusion configuration '%s' fail because of missing slashes", expStr)
		}
		re, err := regexp.Compile(strings.TrimSpace(expStr[1 : len(expStr)-1]))
		if err != nil {
			return fmt.Errorf("cache exclusion configuration '%s' fail because '%s'", expStr, err.Error())
		}
		compiled = append(compiled, re)
	}
	c.compiledExclusions = compiled

	return nil
}

func (r *CachingResolver) LogConfig(logger *slog.Logger) {
	r.cfg.LogConfig(logger)

	logger.Info(fmt.Sprintf("cache entries = %d", r.resultCache.TotalCount()))
}

// Resolve checks if the current query should use the cache and if the result is already in
// the cache and returns it or delegates to the next resolver
func (r *CachingResolver) Resolve(ctx context.Context, request *model.Request) (response *model.Response, err error) {
	ctx, logger := r.log(ctx)

	if !r.IsEnabled() || !r.isRequestCacheable(request) {
		logger.DebugContext(ctx, "skip cache")

		return r.next.Resolve(ctx, request)
	}

	for _, question := range request.Req.Question {
		domain := util.ExtractDomain(question)
		cacheKey := util.GenerateCacheKey(dns.Type(question.Qtype), domain)

		val, ttl := r.getFromCache(ctx, logger, cacheKey, domain)

		if val != nil {
			if logger.Enabled(ctx, slog.LevelDebug) {
				logger.LogAttrs(
					ctx,
					slog.LevelDebug,
					"domain is cached",
					slog.Any(logFieldDomain, util.Obfuscated(domain)),
				)
			}

			val.SetRcode(request.Req, val.Rcode)

			// Adjust TTL
			r.setTTLInCachedResponse(val, ttl)

			if val.Rcode == dns.RcodeSuccess {
				return &model.Response{Res: val, RType: model.ResponseTypeCACHED, Reason: cachedReason}, nil
			}

			return &model.Response{Res: val, RType: model.ResponseTypeCACHED, Reason: "CACHED NEGATIVE"}, nil
		}

		if logger.Enabled(ctx, log.LevelTrace) {
			logger.LogAttrs(
				ctx,
				log.LevelTrace,
				"not in cache: go to next resolver",
				slog.Any(logFieldDomain, util.Obfuscated(domain)),
				slog.String("next_resolver", Name(r.next)),
			)
		}
		response, err = r.next.Resolve(ctx, request)

		if err == nil {
			cacheTTL := r.adjustTTLs(response.Res.Answer)
			r.putInCache(ctx, logger, cacheKey, response, cacheTTL)
		} else {
			return nil, err
		}
	}

	return response, nil
}

func (r *CachingResolver) getFromCache(
	ctx context.Context,
	logger *slog.Logger,
	key, domain string,
) (*dns.Msg, time.Duration) {
	val, ttl := r.resultCache.Get(key)
	if val == nil {
		return nil, 0
	}

	res := new(dns.Msg)

	err := res.Unpack(*val)
	if err != nil {
		logger.LogAttrs(
			ctx,
			slog.LevelError,
			"can't unpack cached entry. Cache malformed?"+err.Error(),
			slog.Any(logFieldDomain, util.Obfuscated(domain)),
		)

		return nil, 0
	}

	return res, ttl
}

// setTTLInCachedResponse ages all records of a cached message by the time the entry has
// already spent in the cache, so that every section counts down instead of repeating the
// TTLs the upstream sent. `ttl` is the entry's remaining lifetime.
func (r *CachingResolver) setTTLInCachedResponse(resp *dns.Msg, ttl time.Duration) {
	// Mirrors putInCache: only a successful, non-empty answer is stored with the smallest
	// answer TTL (see adjustTTLs). Everything else -- NXDOMAIN (even one carrying a CNAME
	// chain) and NODATA -- is stored with the negative cache time. That baseline is what
	// the remaining lifetime has been counting down from.
	baseTTL := r.cfg.CacheTimeNegative.SecondsU32()

	if resp.Rcode == dns.RcodeSuccess && len(resp.Answer) > 0 {
		baseTTL = uint32(math.MaxInt32)
		for _, rr := range resp.Answer {
			baseTTL = min(baseTTL, rr.Header().Ttl)
		}
	}

	var elapsed uint32
	if remaining := uint32(ttl.Seconds()); baseTTL > remaining {
		elapsed = baseTTL - remaining
	}

	for _, section := range [][]dns.RR{resp.Answer, resp.Ns, resp.Extra} {
		for _, rr := range section {
			// An OPT record's TTL field carries flags and the extended rcode, not a lifetime.
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}

			rr.Header().Ttl = max(rr.Header().Ttl, elapsed) - elapsed
		}
	}
}

// isRequestCacheable returns true if the request should be cached
func (r *CachingResolver) isRequestCacheable(request *model.Request) bool {
	// don't cache response if name ends with any exclution
	if questionsMatchAnyExcludedElement(request.Req.Question, r.compiledExclusions) {
		return false
	}
	// shortcut: client-dependent answers bypass caching; add per-view keys and view-aware prefetching if needed.
	if util.GetEdns0Option[*dns.EDNS0_SUBNET](request.Req) != nil {
		return false
	}

	cacheable := true
	ForEach(r.next, func(next Resolver) {
		if _, clientViews := next.(*UpstreamTreeResolver); clientViews {
			cacheable = false
		}
		if ecs, ok := next.(*ECSResolver); ok && (ecs.cfg.IPv4Mask > 0 || ecs.cfg.IPv6Mask > 0) {
			cacheable = false
		}
	})

	return cacheable
}

func questionsMatchAnyExcludedElement(questions []dns.Question, exclutions []*regexp.Regexp) bool {
	for _, q := range questions {
		if matchAnyElementOfArray(q.Name[:len(q.Name)-1], exclutions) {
			return true
		}
	}

	return false
}

func matchAnyElementOfArray(givingText string, arr []*regexp.Regexp) bool {
	loweredGivingText := strings.ToLower(givingText)
	for _, s := range arr {
		if s.MatchString(loweredGivingText) {
			return true
		}
	}

	return false
}

// isResponseCacheable returns true if the response is not truncated and its CD flag isn't set.
func isResponseCacheable(msg *dns.Msg) bool {
	// we don't cache truncated responses and responses with CD flag
	return !msg.Truncated && !msg.CheckingDisabled
}

// packForCache copies the message, strips EDNS0 OPT records (which must never be
// cached) and returns the packed wire bytes.
func packForCache(ctx context.Context, logger *slog.Logger, msg *dns.Msg) ([]byte, error) {
	msgCopy := msg.Copy()
	util.RemoveEdns0Record(msgCopy)

	packed, err := msgCopy.Pack()
	if err != nil {
		logger.ErrorContext(ctx, "error on packing"+err.Error())
	}

	return packed, err
}

// Cached wire bytes cannot preserve trusted-local or synthesized origins, which
// DNSSEC and rebinding checks above the cache need to distinguish on every hit.
func isCacheableResponseType(rType model.ResponseType) bool {
	return rType == model.ResponseTypeRESOLVED
}

func (r *CachingResolver) putInCache(
	ctx context.Context,
	logger *slog.Logger,
	cacheKey string,
	response *model.Response,
	ttl time.Duration,
) {
	if !isCacheableResponseType(response.RType) || !isResponseCacheable(response.Res) {
		return
	}

	packed, err := packForCache(ctx, logger, response.Res)
	if err != nil {
		return
	}

	switch response.Res.Rcode {
	case dns.RcodeSuccess:
		// put value into cache
		r.resultCache.Put(cacheKey, &packed, ttl)
	case dns.RcodeNameError:
		if r.cfg.CacheTimeNegative.IsAboveZero() {
			// put negative cache if result code is NXDOMAIN
			r.resultCache.Put(cacheKey, &packed, r.cfg.CacheTimeNegative.ToDuration())
		}
	}
}

// EntryCount returns the number of currently cached (valid, not expired) entries.
func (r *CachingResolver) EntryCount() int {
	return r.resultCache.TotalCount()
}

// adjustTTLs calculates and returns the min TTL (considers also the min and max cache time)
// for all records from answer or a negative cache time for empty answer
// adjust the TTL in the answer header accordingly
func (r *CachingResolver) adjustTTLs(answer []dns.RR) (ttl time.Duration) {
	minTTL := uint32(math.MaxInt32)

	if len(answer) == 0 {
		return r.cfg.CacheTimeNegative.ToDuration()
	}

	for _, a := range answer {
		// if TTL < mitTTL -> adjust the value, set minTTL
		if r.cfg.MinCachingTime.IsAboveZero() {
			if atomic.LoadUint32(&a.Header().Ttl) < r.cfg.MinCachingTime.SecondsU32() {
				atomic.StoreUint32(&a.Header().Ttl, r.cfg.MinCachingTime.SecondsU32())
			}
		}

		if r.cfg.MaxCachingTime.IsAboveZero() {
			if atomic.LoadUint32(&a.Header().Ttl) > r.cfg.MaxCachingTime.SecondsU32() {
				atomic.StoreUint32(&a.Header().Ttl, r.cfg.MaxCachingTime.SecondsU32())
			}
		}

		headerTTL := atomic.LoadUint32(&a.Header().Ttl)
		if minTTL > headerTTL {
			minTTL = headerTTL
		}
	}

	return time.Duration(minTTL) * time.Second
}

func (r *CachingResolver) FlushCaches(ctx context.Context) {
	_, logger := r.log(ctx)

	logger.DebugContext(ctx, "flush caches")
	r.resultCache.Clear()
}
