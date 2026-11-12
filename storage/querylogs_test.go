package storage

import (
	"context"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestQueryLogsPageCancellation(t *testing.T) {
	resetStorageState()
	t.Cleanup(resetStorageState)
	require.NoError(t, InitDB(filepath.Join(t.TempDir(), "cancel.db")))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	page, err := GetQueryLogsPage(ctx, "", 10, "", "", "", "")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, page)

	conn, err := getDB()
	require.NoError(t, err)
	conn.SetMaxOpenConns(1)
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	raw, err := conn.Conn(t.Context())
	require.NoError(t, err)
	started := false
	require.NoError(t, raw.Raw(func(driverConn any) error {
		sqlite, ok := driverConn.(*sqlite3.SQLiteConn)
		require.True(t, ok)

		return sqlite.RegisterFunc("cancel_scan", func() string {
			started = true
			cancel()

			return "other.test"
		}, false)
	}))
	// Shadow the table with a long scan so cancellation happens inside SQLite,
	// rather than while waiting for a connection or before the query starts.
	_, err = raw.ExecContext(t.Context(), `CREATE TEMP VIEW query_logs AS
WITH RECURSIVE numbers(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM numbers WHERE n < 100000000)
SELECT n AS id, 0 AS timestamp, '' AS client_ip, '' AS client_name,
cancel_scan() AS domain, 'A' AS query_type, 'RESOLVED' AS response_type,
'' AS reason, 0 AS duration_ms FROM numbers`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	page, err = GetQueryLogsPage(ctx, "", 10, "missing.test", "", "", "")
	require.True(t, started)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, page)
	_, err = conn.ExecContext(t.Context(), "DROP VIEW temp.query_logs")
	require.NoError(t, err)
	page, err = GetQueryLogsPage(t.Context(), "", 10, "", "", "", "")
	require.NoError(t, err)
	require.Empty(t, page.Entries)
}

func TestQueryLogsCursorPreservesInt64(t *testing.T) {
	for _, id := range []int64{1<<31 + 1, 1<<53 + 1, math.MaxInt64} {
		t.Run(strconv.FormatInt(id, 10), func(t *testing.T) {
			cursor := formatQueryLogsCursor(&QueryLogReadEntry{ID: id})
			got, err := parseQueryLogsCursor(cursor)
			require.NoError(t, err)
			require.Equal(t, id, got)
		})
	}
	t.Run("overflow", func(t *testing.T) {
		_, err := parseQueryLogsCursor("9223372036854775808")
		require.Error(t, err)
	})
}

func TestQueryLogsPagePreservesInt64(t *testing.T) {
	resetStorageState()
	t.Cleanup(resetStorageState)
	require.NoError(t, InitDB(filepath.Join(t.TempDir(), "large-ids.db")))
	conn, err := getDB()
	require.NoError(t, err)
	ids := []int64{1<<31 + 1, 1<<53 + 1, math.MaxInt64}
	for _, id := range ids {
		_, err := conn.ExecContext(t.Context(), `
INSERT INTO query_logs (id, timestamp, client_ip, client_name, domain, query_type, response_type, reason, duration_ms)
VALUES (?, 0, '', '', 'large-id.test', 'A', 'RESOLVED', '', 0)`, id)
		require.NoError(t, err)
	}
	cursor := ""
	for i, id := range slices.Backward(ids) {
		page, err := GetQueryLogsPage(t.Context(), cursor, 1, "", "", "", "")
		require.NoError(t, err)
		require.Len(t, page.Entries, 1)
		require.Equal(t, id, page.Entries[0].ID)
		require.Equal(t, i > 0, page.HasMore)
		cursor = page.NextCursor
	}
}
