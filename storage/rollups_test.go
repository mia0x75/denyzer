package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWindowRollupsAcrossMidnightDST(t *testing.T) {
	const childFlag = "DENYZER_TEST_MIDNIGHT_DST"
	if os.Getenv(childFlag) != "1" {
		// Isolate a regressed infinite loop and the process-local timezone from the rest of the suite.
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWindowRollupsAcrossMidnightDST$")
		cmd.Env = append(os.Environ(), childFlag+"=1", "TZ=America/Santiago")
		output, err := cmd.CombinedOutput()
		require.NoError(t, ctx.Err(), "maintenance did not terminate: %s", output)
		require.NoError(t, err, "%s", output)

		return
	}

	zone, err := time.LoadLocation("America/Santiago")
	require.NoError(t, err)
	for _, days := range []int{1, 4} {
		t.Run(fmt.Sprintf("advance_%d_days", days), func(t *testing.T) {
			resetStorageState()
			t.Cleanup(resetStorageState)
			require.NoError(t, InitDB(filepath.Join(t.TempDir(), "dst.db")))
			now := time.Date(2026, time.September, 11, 12, 0, 0, 0, zone)
			later := now.AddDate(0, 0, days)
			batch, err := NewBatch()
			require.NoError(t, err)
			defer RollbackBatch(batch)
			for _, spec := range WindowSpecs() {
				start := WindowStart(now, spec.Days)
				end := WindowStart(later, spec.Days)
				for _, dimension := range []string{DimensionDomain, DimensionClient} {
					for _, bucket := range []int64{start - 1, start, start + (end-start)/2, end} {
						require.NoError(t, UpsertMetrics(batch, []MetricInput{{
							Dimension: dimension, Granularity: GranularityDay,
							BucketStart: bucket,
							Entity:      spec.Granularity, EntityLabel: spec.Granularity,
							Delta: rollupTestCounters(1),
						}}))
					}
				}
			}
			require.NoError(t, CommitBatch(batch))
			require.NoError(t, MaintainWindowRollups(now))
			require.NoError(t, MaintainWindowRollups(later))
			require.True(t, WindowRollupsCurrent(later))
			assertRollupCounters(t, rollupTestCounters(1))
			require.NoError(t, MaintainWindowRollups(later))
			assertRollupCounters(t, rollupTestCounters(1))
			windowRollupsReady.Store(false)
			require.NoError(t, MaintainWindowRollups(later))
			assertRollupCounters(t, rollupTestCounters(1))
		})
	}
}

func TestWindowRollupExpiryRollback(t *testing.T) {
	for _, failure := range []struct {
		name    string
		trigger string
	}{
		{"client_update", `BEFORE UPDATE ON metrics
WHEN OLD.granularity = 'window_7d' AND OLD.dimension = 'client'`},
		{"watermark", `BEFORE INSERT ON metrics
WHEN NEW.dimension = 'meta' AND NEW.entity = 'window_30d'`},
	} {
		t.Run(failure.name, func(t *testing.T) {
			resetStorageState()
			t.Cleanup(resetStorageState)
			require.NoError(t, InitDB(filepath.Join(t.TempDir(), "rollback.db")))
			now := time.Date(2026, time.November, 13, 12, 0, 0, 0, time.UTC)
			later := now.AddDate(0, 0, 3)
			batch, err := NewBatch()
			require.NoError(t, err)
			defer RollbackBatch(batch)
			for _, spec := range WindowSpecs() {
				for _, dimension := range []string{DimensionDomain, DimensionClient} {
					for _, bucket := range []int64{WindowStart(now, spec.Days), WindowStart(later, spec.Days)} {
						require.NoError(t, UpsertMetrics(batch, []MetricInput{{
							Dimension: dimension, Granularity: GranularityDay, BucketStart: bucket,
							Entity: spec.Granularity, EntityLabel: spec.Granularity, Delta: rollupTestCounters(1),
						}}))
					}
				}
			}
			require.NoError(t, CommitBatch(batch))
			require.NoError(t, MaintainWindowRollups(now))
			conn, err := getDB()
			require.NoError(t, err)
			_, err = conn.ExecContext(t.Context(), "CREATE TRIGGER fail_expiry "+failure.trigger+
				" BEGIN SELECT RAISE(ABORT, 'expiry failed'); END")
			require.NoError(t, err)
			require.ErrorContains(t, MaintainWindowRollups(later), "expiry failed")
			require.False(t, WindowRollupsReady())
			assertRollupCounters(t, rollupTestCounters(2))
			require.True(t, WindowRollupsCurrent(now))
			_, err = conn.ExecContext(t.Context(), "DROP TRIGGER fail_expiry")
			require.NoError(t, err)
			require.NoError(t, MaintainWindowRollups(later))
			require.True(t, WindowRollupsCurrent(later))
			assertRollupCounters(t, rollupTestCounters(1))
		})
	}
}

func rollupTestCounters(scale uint64) MetricsDelta {
	return MetricsDelta{
		Total: 45 * scale, Blocked: scale, Cached: 2 * scale, Forwarded: 3 * scale,
		Filtered: 4 * scale, Local: 5 * scale, Other: 6 * scale,
		Dropped: 7 * scale, Errors: 8 * scale, TotalLatencyMs: 90 * scale,
	}
}

func assertRollupCounters(t *testing.T, want MetricsDelta) {
	t.Helper()
	conn, err := getDB()
	require.NoError(t, err)
	for _, spec := range WindowSpecs() {
		for _, dimension := range []string{DimensionDomain, DimensionClient} {
			var got MetricsDelta
			err := conn.QueryRowContext(t.Context(), `SELECT total, blocked, cached, forwarded,
filtered, local, other, dropped, errors, total_latency_ms FROM metrics
WHERE granularity = ? AND dimension = ? AND bucket_start = 0 AND entity = ?`,
				spec.Granularity, dimension, spec.Granularity).Scan(
				&got.Total, &got.Blocked, &got.Cached, &got.Forwarded, &got.Filtered,
				&got.Local, &got.Other, &got.Dropped, &got.Errors, &got.TotalLatencyMs)
			require.NoError(t, err)
			require.Equal(t, want, got, "%s/%s", spec.Granularity, dimension)
		}
	}
}

func TestWindowRollupsRecoverAfterPartialFailure(t *testing.T) {
	resetStorageState()
	t.Cleanup(resetStorageState)
	path := filepath.Join(t.TempDir(), "windows.db")
	require.NoError(t, InitDB(path))
	now := time.Now()
	writeDay := func(total uint64) {
		t.Helper()
		batch, err := NewBatch()
		require.NoError(t, err)
		defer RollbackBatch(batch)
		require.NoError(t, UpsertMetrics(batch, []MetricInput{{
			Dimension: DimensionDomain, Granularity: GranularityDay,
			BucketStart: DayBucket(now), Entity: "recover.test", EntityLabel: "recover.test",
			Delta: MetricsDelta{Total: total},
		}}))
		require.NoError(t, CommitBatch(batch))
	}
	assertWindows := func(total uint64) {
		t.Helper()
		for _, spec := range WindowSpecs() {
			rows, err := GetTopEntities(DimensionDomain, spec.Granularity, 0, 0)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, total, rows[0].Total, spec.Granularity)
		}
	}
	writeDay(1)
	require.NoError(t, MaintainWindowRollups(now))
	assertWindows(1)
	// Simulate restart with valid watermarks and day data missing from the windows.
	require.NoError(t, CloseDB())
	require.NoError(t, InitDB(path))
	require.False(t, WindowRollupsReady())
	writeDay(1)
	conn, err := getDB()
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `CREATE TRIGGER fail_second_window BEFORE INSERT ON metrics
WHEN NEW.dimension = 'meta' AND NEW.entity = 'window_30d'
BEGIN SELECT RAISE(ABORT, 'second window failed'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, MaintainWindowRollups(now), "second window failed")
	require.False(t, WindowRollupsReady())
	assertWindows(1)
	writeDay(1)
	_, err = conn.ExecContext(t.Context(), `DROP TRIGGER fail_second_window`)
	require.NoError(t, err)
	require.NoError(t, MaintainWindowRollups(now))
	assertWindows(3)
	require.NoError(t, MaintainWindowRollups(now))
	assertWindows(3)
}

func TestWindowRollupsCurrentFollowsDayBoundary(t *testing.T) {
	resetStorageState()
	t.Cleanup(resetStorageState)
	require.NoError(t, InitDB(filepath.Join(t.TempDir(), "current.db")))

	now := time.Now()
	require.False(t, WindowRollupsCurrent(now))

	require.NoError(t, MaintainWindowRollups(now))
	require.True(t, WindowRollupsCurrent(now))

	// Midnight passed but maintenance has not run yet: the stored watermarks
	// still cover the previous window.
	next := now.AddDate(0, 0, 1)
	require.False(t, WindowRollupsCurrent(next))

	require.NoError(t, MaintainWindowRollups(next))
	require.True(t, WindowRollupsCurrent(next))
}

func TestWindowMaintenanceWaitsForUpdate(t *testing.T) {
	resetStorageState()
	t.Cleanup(resetStorageState)
	require.NoError(t, InitDB(filepath.Join(t.TempDir(), "exclusive.db")))
	started := make(chan struct{})
	finished := make(chan error, 1)
	require.NoError(t, WithWindowRollupUpdate(func() error {
		go func() {
			close(started)
			finished <- MaintainWindowRollups(time.Now())
		}()
		<-started
		select {
		case err := <-finished:
			t.Errorf("maintenance crossed an active update: %v", err)
		case <-time.After(20 * time.Millisecond):
		}

		return nil
	}))
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not resume after update")
	}
}
