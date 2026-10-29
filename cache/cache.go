package cache

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jellydator/ttlcache/v3"
)

// Options configures an in-memory expiring cache.
type Options struct {
	// CleanupInterval is accepted for compatibility with the previous cache
	// implementation. ttlcache schedules its own janitor from the nearest
	// expiry, so the value is informational only.
	CleanupInterval time.Duration
	// MaxSize bounds the number of entries (0 means unlimited).
	MaxSize uint
	// OnCacheHitFn is called on a successful Get.
	OnCacheHitFn func(key string)
	// OnCacheMissFn is called when a Get finds no live entry.
	OnCacheMissFn func(key string)
	// OnAfterPutFn is called after an entry is stored, with the current count.
	OnAfterPutFn func(count int)
}

// OnExpirationCallback is invoked shortly before an entry expires. It may
// return a fresh value together with a positive TTL to keep the entry alive,
// or nil to let the entry be removed.
type OnExpirationCallback[T any] func(ctx context.Context, key string) (*T, time.Duration)

// ReloadEntryFn reloads a cache entry by key. It is used by prefetching to
// refresh frequently queried entries transparently through a ttlcache Loader.
type ReloadEntryFn[T any] func(ctx context.Context, key string) (*T, time.Duration)

// PrefetchingOptions configures the prefetching behaviour of a cache.
type PrefetchingOptions[T any] struct {
	// ReloadFn reloads a missing-but-hot entry from upstream.
	ReloadFn ReloadEntryFn[T]
	// PrefetchExpires is the sliding window during which queries are counted.
	PrefetchExpires time.Duration
	// PrefetchThreshold is the minimum query count within PrefetchExpires for an
	// entry to be eligible for prefetching. 0 prefetches every known entry.
	PrefetchThreshold int
	// MaxItemsCount bounds the number of tracked query counters (0 means
	// unlimited).
	MaxItemsCount int
	// OnPrefetchEntryReloaded is called after an entry was reloaded by prefetch.
	OnPrefetchEntryReloaded func(key string)
	// OnPrefetchCacheHit is called when a prefetched entry is served.
	OnPrefetchCacheHit func(key string)
}

// ttlCache is a ttlcache-backed ExpiringCache. ttlcache provides per-item TTL,
// synchronous visibility and eviction callbacks, so no margin/janitor glue is
// needed on top of it.
type ttlCache[T any] struct {
	cache     *ttlcache.Cache[string, T]
	onExpired OnExpirationCallback[T]
	onHit     func(key string)
	onMiss    func(key string)
	onPut     func(count int)
}

// NewCache creates an in-memory TTL cache with optional capacity and callbacks.
func NewCache[T any](opts Options) *ttlCache[T] {
	return NewCacheWithOnExpired[T](opts, nil)
}

// NewCacheWithOnExpired creates a TTL cache backed by ttlcache. Entries are
// removed by ttlcache when their TTL elapses; onExpired, when set, is consulted
// at eviction time and may return a fresh value with a positive TTL to keep the
// entry alive. The caller is responsible for starting the cache's janitor via
// Start.
func NewCacheWithOnExpired[T any](opts Options, onExpired OnExpirationCallback[T]) *ttlCache[T] {
	c := &ttlCache[T]{
		onExpired: onExpired,
		onHit:     opts.OnCacheHitFn,
		onMiss:    opts.OnCacheMissFn,
		onPut:     opts.OnAfterPutFn,
	}

	cacheOpts := []ttlcache.Option[string, T]{ttlcache.WithDisableTouchOnHit[string, T]()}
	if opts.MaxSize > 0 {
		cacheOpts = append(cacheOpts, ttlcache.WithCapacity[string, T](uint64(opts.MaxSize)))
	}

	c.cache = ttlcache.New(cacheOpts...)
	c.cache.OnEviction(c.handleEviction)

	return c
}

// handleEviction re-inserts keep-alive entries whose callback returns a fresh
// value, and otherwise lets ttlcache drop them.
func (c *ttlCache[T]) handleEviction(
	ctx context.Context, reason ttlcache.EvictionReason, item *ttlcache.Item[string, T],
) {
	if c.onExpired == nil || reason != ttlcache.EvictionReasonExpired {
		return
	}

	val, ttl := c.onExpired(ctx, item.Key())
	if val != nil && ttl > 0 {
		c.cache.Set(item.Key(), *val, ttl)
	}
}

// Start launches the background janitor that sweeps expired entries. It stops
// automatically when ctx is cancelled.
func (c *ttlCache[T]) Start(ctx context.Context) {
	go c.cache.Start()

	go func() {
		<-ctx.Done()
		c.cache.Stop()
	}()
}

// Stop terminates the background janitor.
func (c *ttlCache[T]) Stop() {
	c.cache.Stop()
}

func (c *ttlCache[T]) Put(key string, val *T, expiration time.Duration) {
	if expiration <= 0 || val == nil {
		return
	}

	c.cache.Set(key, *val, expiration)

	if c.onPut != nil {
		c.onPut(c.TotalCount())
	}
}

func (c *ttlCache[T]) Get(key string) (*T, time.Duration) {
	item := c.cache.Get(key)
	if item == nil {
		if c.onMiss != nil {
			c.onMiss(key)
		}

		return nil, 0
	}

	if c.onHit != nil {
		c.onHit(key)
	}

	val := item.Value()
	remaining := max(time.Until(item.ExpiresAt()), 0)

	return &val, remaining
}

func (c *ttlCache[T]) TotalCount() int {
	return c.cache.Len()
}

func (c *ttlCache[T]) Del(key string) {
	c.cache.Delete(key)
}

func (c *ttlCache[T]) Clear() {
	c.cache.DeleteAll()
}

// prefetchingCache wraps a TTL cache and, through a ttlcache Loader, refreshes
// frequently queried entries transparently when they are looked up after
// expiring. Cold keys are left to the caller's normal resolution path.
type prefetchingCache[T any] struct {
	*ttlCache[T]

	counts                  *ttlcache.Cache[string, *atomic.Uint32]
	reloadFn                ReloadEntryFn[T]
	prefetchExpires         time.Duration
	prefetchThreshold       int
	onPrefetchEntryReloaded func(key string)
	onPrefetchCacheHit      func(key string)

	// ctx backs reloadFn, which is invoked from Get/ttlcache callbacks that do
	// not carry a request context.
	//
	//nolint:containedctx
	ctx context.Context
}

// NewPrefetchingCache creates a cache that reloads hot entries via a Loader.
func NewPrefetchingCache[T any](
	ctx context.Context, opts Options, popts PrefetchingOptions[T],
) *prefetchingCache[T] {
	base := NewCacheWithOnExpired[T](opts, nil)

	pc := &prefetchingCache[T]{
		ttlCache:                base,
		reloadFn:                popts.ReloadFn,
		prefetchExpires:         popts.PrefetchExpires,
		prefetchThreshold:       popts.PrefetchThreshold,
		onPrefetchEntryReloaded: popts.OnPrefetchEntryReloaded,
		onPrefetchCacheHit:      popts.OnPrefetchCacheHit,
		ctx:                     ctx,
	}

	countOpts := []ttlcache.Option[string, *atomic.Uint32]{
		ttlcache.WithDisableTouchOnHit[string, *atomic.Uint32](),
	}
	if popts.MaxItemsCount > 0 {
		countOpts = append(countOpts, ttlcache.WithCapacity[string, *atomic.Uint32](uint64(popts.MaxItemsCount)))
	}

	pc.counts = ttlcache.New(countOpts...)
	go pc.counts.Start()

	base.Start(ctx)

	return pc
}

// Get counts the lookup, serves a hit or prefetches a hot expired entry.
func (c *prefetchingCache[T]) Get(key string) (*T, time.Duration) {
	// A key is only eligible for prefetching once it has been seen before: the
	// first lookup of a new key must go through the caller's normal resolution
	// path, not the loader.
	previousCount := c.trackQuery(key)

	item := c.cache.Get(key)
	if item != nil {
		if c.onPrefetchCacheHit != nil {
			c.onPrefetchCacheHit(key)
		}

		val := item.Value()
		remaining := max(time.Until(item.ExpiresAt()), 0)

		return &val, remaining
	}

	if c.onMiss != nil {
		c.onMiss(key)
	}

	// Prefetch only entries that were seen often enough. Cold keys fall through
	// to the caller, which resolves them normally.
	if previousCount == 0 || !c.shouldPrefetch(key) || c.reloadFn == nil {
		return nil, 0
	}

	val, ttl := c.reloadFn(c.ctx, key)
	if val == nil || ttl <= 0 {
		return nil, 0
	}

	c.cache.Set(key, *val, ttl)

	if c.onPrefetchEntryReloaded != nil {
		c.onPrefetchEntryReloaded(key)
	}

	if c.onPut != nil {
		c.onPut(c.TotalCount())
	}

	return val, ttl
}

// put stores a value without counting it as a query.
func (c *prefetchingCache[T]) Put(key string, val *T, expiration time.Duration) {
	c.ttlCache.Put(key, val, expiration)
}

// trackQuery increments the sliding-window query counter for a key and returns
// the count observed before this lookup (0 for the first ever query).
func (c *prefetchingCache[T]) trackQuery(key string) uint32 {
	item := c.counts.Get(key)

	var (
		counter   *atomic.Uint32
		previous  uint32
		hadBefore = item != nil
	)

	if hadBefore {
		counter = item.Value()
		previous = counter.Load()
	} else {
		counter = new(atomic.Uint32)
	}

	counter.Add(1)
	c.counts.Set(key, counter, c.prefetchExpires)

	return previous
}

// shouldPrefetch reports whether a key was queried often enough to be reloaded.
func (c *prefetchingCache[T]) shouldPrefetch(key string) bool {
	if c.prefetchThreshold == 0 {
		return true
	}

	item := c.counts.Get(key)
	if item == nil {
		return false
	}

	return int64(item.Value().Load()) > int64(c.prefetchThreshold)
}

// Clear removes cached answers and the query counters.
func (c *prefetchingCache[T]) Clear() {
	c.ttlCache.Clear()
	c.counts.DeleteAll()
}
