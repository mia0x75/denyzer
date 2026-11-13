package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStorageMaintenanceCancellation(t *testing.T) {
	resetStorageState()
	t.Cleanup(resetStorageState)
	path := filepath.Join(t.TempDir(), "shutdown.db")
	require.NoError(t, InitDB(path))
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	lock, err := conn.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Close()) })
	for _, tc := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"batch", func(ctx context.Context) error {
			batch, err := NewBatchContext(ctx)
			RollbackBatch(batch)

			return err
		}},
		{"cleanup", func(ctx context.Context) error {
			_, err := CleanUpContext(ctx, 1)

			return err
		}},
		{"rollups", func(ctx context.Context) error {
			return MaintainWindowRollupsContext(ctx, time.Now())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := lock.ExecContext(t.Context(), "BEGIN IMMEDIATE")
			require.NoError(t, err)
			t.Cleanup(func() { _, _ = lock.ExecContext(context.Background(), "ROLLBACK") })
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			require.ErrorIs(t, tc.run(ctx), context.DeadlineExceeded)
			require.Less(t, time.Since(started), 500*time.Millisecond)
		})
	}
}

func TestCleanupRetriesWriteContention(t *testing.T) {
	resetStorageState()
	t.Cleanup(resetStorageState)
	require.NoError(t, InitDB(filepath.Join(t.TempDir(), "contention.db")))
	batch, err := NewBatchContext(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { RollbackBatch(batch) })
	result := make(chan error, 1)
	go func() {
		_, err := CleanUpContext(t.Context(), 1)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("cleanup returned before the write lock was released: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, CommitBatch(batch))
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish after the write lock was released")
	}
}
