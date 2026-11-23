package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGlobalStatsCacheHitRate(t *testing.T) {
	assert.Zero(t, GlobalStats{}.CacheHitRate(), "no lookups must not divide by zero")
	assert.InDelta(t, 0.25, GlobalStats{Cached: 1, Forwarded: 3}.CacheHitRate(), 0.0001)
}

func TestGlobalStatsAverageResponseMs(t *testing.T) {
	assert.Equal(t, uint64(0), GlobalStats{}.AverageResponseMs(), "no answered queries must not divide by zero")

	// Dropped and errored queries are excluded from both the divisor and the
	// latency sum by contract.
	assert.Equal(t, uint64(10), GlobalStats{
		Total: 10, Dropped: 2, Errors: 1, TotalLatencyMs: 70,
	}.AverageResponseMs())

	// The answered count can never exceed the total.
	assert.Equal(t, uint64(0), GlobalStats{
		Total: 2, Dropped: 5, Errors: 5, TotalLatencyMs: 70,
	}.AverageResponseMs())
}

func TestStatCategory(t *testing.T) {
	for responseType, want := range map[string]string{
		"CACHED":      StatCached,
		"RESOLVED":    StatForwarded,
		"CONDITIONAL": StatForwarded,
		"BLOCKED":     StatBlocked,
		"REBIND":      StatBlocked,
		"FILTERED":    StatFiltered,
		"NOTFQDN":     StatFiltered,
		"CUSTOMDNS":   StatLocal,
		"HOSTSFILE":   StatLocal,
		"SPECIAL":     StatLocal,
		"SYNTHESIZED": StatLocal,
		"BOGUS":       StatError,
		"WEIRD":       StatOther,
		"":            StatOther,
	} {
		assert.Equal(t, want, StatCategory(responseType), "response type %q", responseType)
	}
}

func TestClientEntity(t *testing.T) {
	entity, label := ClientEntity("10.0.0.1", "router")
	assert.Equal(t, "router", entity)
	assert.Equal(t, label, entity)

	entity, label = ClientEntity("10.0.0.2", "   ")
	assert.Equal(t, "10.0.0.2", entity, "an unnamed client falls back to its IP")
	assert.Equal(t, "10.0.0.2", label)
}

func TestInWindow(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

	cases := map[string]struct {
		ts   time.Time
		want bool
	}{
		"window start":  {ts: now.AddDate(0, 0, -6), want: true},
		"before window": {ts: now.AddDate(0, 0, -7), want: false},
		"today":         {ts: now, want: true},
		"future day":    {ts: now.AddDate(0, 0, 1), want: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, InWindow(tc.ts, now, 7))
		})
	}
}

func TestSumGlobalStats(t *testing.T) {
	resetStorageState()
	t.Cleanup(resetStorageState)
	require.NoError(t, InitDB(filepath.Join(t.TempDir(), "sum.db")))
	conn, err := getDB()
	require.NoError(t, err)

	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC).Unix()
	for offset, total := range map[int]uint64{0: 10, 1: 32} {
		_, err := conn.ExecContext(t.Context(), `
INSERT INTO metrics (granularity, dimension, bucket_start, entity, total, cached, total_latency_ms)
VALUES ('day', 'global', ?, '', ?, ?, ?)`, day+int64(offset)*86400, total, total/2, total*5)
		require.NoError(t, err)
	}

	stats, err := SumGlobalStats(GranularityDay, day, day+86400)
	require.NoError(t, err)
	assert.Equal(t, uint64(42), stats.Total)
	assert.Equal(t, uint64(21), stats.Cached)
	assert.Equal(t, uint64(210), stats.TotalLatencyMs)

	empty, err := SumGlobalStats(GranularityDay, day+2*86400, day+2*86400)
	require.NoError(t, err)
	assert.Equal(t, GlobalStats{}, empty)
}
