package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCache_BasicOperations(t *testing.T) {
	c := NewCache[string](Options{CleanupInterval: time.Hour})
	c.Start(t.Context())
	defer c.Stop()

	val := "entry"
	c.Put("k", &val, time.Hour)

	retrieved, ttl := c.Get("k")
	require.NotNil(t, retrieved)
	assert.Equal(t, "entry", *retrieved)
	assert.Positive(t, ttl)

	assert.Equal(t, 1, c.TotalCount())

	c.Clear()
	assert.Equal(t, 0, c.TotalCount())
}

func TestNewCache_ZeroExpirationNotCached(t *testing.T) {
	c := NewCache[string](Options{})
	c.Start(t.Context())
	defer c.Stop()

	val := "entry"
	c.Put("k", &val, 0)

	got, _ := c.Get("k")
	assert.Nil(t, got)
	assert.Equal(t, 0, c.TotalCount())
}

func TestNewCache_Callbacks(t *testing.T) {
	var hits atomic.Int32
	var misses atomic.Int32
	var puts atomic.Int32

	c := NewCache[string](Options{
		CleanupInterval: time.Hour,
		OnCacheHitFn:    func(key string) { hits.Add(1) },
		OnCacheMissFn:   func(key string) { misses.Add(1) },
		OnAfterPutFn:    func(count int) { puts.Add(1) },
	})
	c.Start(t.Context())
	defer c.Stop()

	val := "entry"
	c.Put("k", &val, time.Hour)

	retrieved, _ := c.Get("k")
	require.NotNil(t, retrieved)

	c.Get("missing")

	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, int32(1), misses.Load())
	assert.Equal(t, int32(1), puts.Load())
}

func TestNewCache_Expires(t *testing.T) {
	c := NewCache[string](Options{CleanupInterval: time.Millisecond})
	c.Start(t.Context())
	defer c.Stop()

	val := "entry"
	c.Put("k", &val, 5*time.Millisecond)

	require.Eventually(t, func() bool {
		return c.cache.Metrics().Evictions == 1
	}, time.Second, time.Millisecond)

	got, _ := c.Get("k")
	assert.Nil(t, got)
}

func TestNewCache_CleanupStops(t *testing.T) {
	for _, alreadyCanceled := range []bool{false, true} {
		name := "immediate stop"
		if alreadyCanceled {
			name = "already canceled context"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if alreadyCanceled {
				cancel()
			}
			c := NewCache[string](Options{})
			c.Start(ctx)
			if !alreadyCanceled {
				c.Stop()
			}
			select {
			case <-c.cleanupDone:
			case <-time.After(time.Second):
				t.Fatal("cache cleanup did not stop")
			}
			c.Stop()
		})
	}
}

func TestNewCacheWithOnExpired_KeepAlive(t *testing.T) {
	var calls atomic.Int32

	c := NewCacheWithOnExpired[string](Options{CleanupInterval: time.Millisecond}, func(_ context.Context, key string) (*string, time.Duration) {
		calls.Add(1)

		fresh := "fresh-" + key

		return &fresh, 20 * time.Millisecond
	})
	c.Start(t.Context())
	defer c.Stop()

	val := "initial"
	c.Put("k", &val, 5*time.Millisecond)

	// The entry stays served from cache across refreshes.
	require.Eventually(t, func() bool {
		got, _ := c.Get("k")

		return got != nil && *got == "fresh-k"
	}, time.Second, time.Millisecond)

	assert.Positive(t, calls.Load(), "onExpired should have been invoked")
}

func TestNewCacheWithOnExpired_Remove(t *testing.T) {
	c := NewCacheWithOnExpired[string](Options{CleanupInterval: time.Millisecond}, func(_ context.Context, _ string) (*string, time.Duration) {
		return nil, 0
	})
	c.Start(t.Context())
	defer c.Stop()

	val := "initial"
	c.Put("k", &val, 5*time.Millisecond)

	require.Eventually(t, func() bool {
		retrieved, _ := c.Get("k")

		return retrieved == nil
	}, time.Second, time.Millisecond)
}

func TestNewPrefetchingCache_ReloadsHotEntry(t *testing.T) {
	var reloads atomic.Int32

	c := NewPrefetchingCache[string](t.Context(), Options{CleanupInterval: time.Millisecond}, PrefetchingOptions[string]{
		PrefetchExpires:   time.Minute,
		PrefetchThreshold: 0, // every known entry is eligible
		ReloadFn: func(_ context.Context, _ string) (*string, time.Duration) {
			reloads.Add(1)
			fresh := "reloaded"

			return &fresh, 10 * time.Millisecond
		},
	})
	defer c.Stop()

	val := "initial"
	c.Put("k", &val, 5*time.Millisecond)

	// First Get counts the query; the entry is still live.
	got, _ := c.Get("k")
	require.NotNil(t, got)

	// After expiry the Loader transparently reloads the hot entry.
	require.Eventually(t, func() bool {
		got, _ := c.Get("k")

		return got != nil && *got == "reloaded"
	}, time.Second, time.Millisecond)

	assert.Positive(t, reloads.Load(), "reload must have run")
}

func TestNewPrefetchingCache_LeavesColdKeysToCaller(t *testing.T) {
	var reloads atomic.Int32

	c := NewPrefetchingCache[string](t.Context(), Options{CleanupInterval: time.Millisecond}, PrefetchingOptions[string]{
		PrefetchExpires:   time.Minute,
		PrefetchThreshold: 5,
		ReloadFn: func(_ context.Context, _ string) (*string, time.Duration) {
			reloads.Add(1)
			fresh := "reloaded"

			return &fresh, 10 * time.Millisecond
		},
	})
	defer c.Stop()

	// Never put and never queried enough: a miss must not reload.
	got, _ := c.Get("cold")
	assert.Nil(t, got)
	assert.Zero(t, reloads.Load())
}

func TestNewPrefetchingCache_CoalescesConcurrentReloads(t *testing.T) {
	const requests = 10

	var (
		reloads atomic.Int32
		misses  atomic.Int32
	)
	reloadStarted := make(chan struct{}, 1)
	releaseReload := make(chan struct{})
	c := NewPrefetchingCache[string](t.Context(), Options{
		CleanupInterval: time.Millisecond,
		OnCacheMissFn:   func(string) { misses.Add(1) },
	}, PrefetchingOptions[string]{
		PrefetchExpires:   time.Minute,
		PrefetchThreshold: 0,
		ReloadFn: func(_ context.Context, _ string) (*string, time.Duration) {
			reloads.Add(1)
			select {
			case reloadStarted <- struct{}{}:
			default:
			}
			<-releaseReload
			fresh := "reloaded"

			return &fresh, time.Minute
		},
	})
	defer c.Stop()

	initial := "initial"
	c.Put("k", &initial, time.Minute)
	_, _ = c.Get("k") // Make the key eligible for prefetching.
	c.Del("k")

	results := make(chan *string, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			got, _ := c.Get("k")
			results <- got
		})
	}

	select {
	case <-reloadStarted:
	case <-time.After(time.Second):
		t.Fatal("prefetch reload did not start")
	}
	require.Eventually(t, func() bool { return misses.Load() == requests }, time.Second, time.Millisecond)
	close(releaseReload)
	wg.Wait()
	close(results)

	for got := range results {
		require.NotNil(t, got)
		assert.Equal(t, "reloaded", *got)
	}
	assert.Equal(t, int32(1), reloads.Load())
}

func TestNewPrefetchingCache_ReturnsRemainingReloadTTL(t *testing.T) {
	const reloadTTL = time.Second

	t.Run("returns remaining TTL", func(t *testing.T) {
		value, ttl := testPrefetchReloadTTL(t, reloadTTL, 50*time.Millisecond)
		require.NotNil(t, value)
		assert.Equal(t, "reloaded", *value)
		assert.Less(t, ttl, reloadTTL-25*time.Millisecond)
	})
	t.Run("does not return expired value", func(t *testing.T) {
		value, ttl := testPrefetchReloadTTL(t, reloadTTL, reloadTTL+25*time.Millisecond)
		assert.Nil(t, value)
		assert.Zero(t, ttl)
	})
}

func testPrefetchReloadTTL(
	t *testing.T,
	reloadTTL time.Duration,
	delay time.Duration,
) (*string, time.Duration) {
	t.Helper()
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCallback) }) }
	c := NewPrefetchingCache[string](t.Context(), Options{CleanupInterval: time.Hour}, PrefetchingOptions[string]{
		PrefetchExpires:   time.Minute,
		PrefetchThreshold: 0,
		ReloadFn: func(_ context.Context, _ string) (*string, time.Duration) {
			value := "reloaded"

			return &value, reloadTTL
		},
		OnPrefetchEntryReloaded: func(string) {
			close(callbackStarted)
			<-releaseCallback
		},
	})
	defer c.Stop()
	defer release()

	initial := "initial"
	c.Put("k", &initial, time.Minute)
	_, _ = c.Get("k") // Make the key eligible for prefetching.
	c.Del("k")

	type result struct {
		value *string
		ttl   time.Duration
	}
	got := make(chan result, 1)
	go func() {
		value, ttl := c.Get("k")
		got <- result{value: value, ttl: ttl}
	}()

	select {
	case <-callbackStarted:
	case <-time.After(time.Second):
		t.Fatal("reload callback did not start")
	}
	time.Sleep(delay)
	// An in-flight normal lookup can replace the entry while reload callbacks run.
	replacement := "replacement"
	c.Put("k", &replacement, time.Hour)
	release()

	select {
	case reloaded := <-got:
		return reloaded.value, reloaded.ttl
	case <-time.After(time.Second):
		t.Fatal("cache Get did not return")

		return nil, 0
	}
}

func TestNewPrefetchingCache_CleanupStopsWithLifecycle(t *testing.T) {
	for _, stopExplicitly := range []bool{false, true} {
		name := "context cancellation"
		if stopExplicitly {
			name = "explicit stop"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			c := NewPrefetchingCache[string](ctx, Options{CleanupInterval: time.Millisecond}, PrefetchingOptions[string]{
				PrefetchExpires: time.Millisecond,
			})
			c.trackQuery("k")

			require.Eventually(t, func() bool { return c.counts.Metrics().Evictions == 1 }, time.Second, time.Millisecond)
			if stopExplicitly {
				c.Stop()
			} else {
				cancel()
			}

			for name, done := range map[string]<-chan struct{}{
				"answer cache cleanup": c.cleanupDone,
				"count cache cleanup":  c.countsCleanupDone,
			} {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatalf("%s did not stop", name)
				}
			}

			c.Stop()
			cancel()
		})
	}
}
