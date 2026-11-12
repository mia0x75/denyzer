package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
