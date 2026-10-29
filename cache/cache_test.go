package cache

import (
	"context"
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
		got, _ := c.Get("k")

		return got == nil
	}, time.Second, time.Millisecond)
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
