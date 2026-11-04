package storage

import (
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

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
		page, err := GetQueryLogsPage(cursor, 1, "", "", "", "")
		require.NoError(t, err)
		require.Len(t, page.Entries, 1)
		require.Equal(t, id, page.Entries[0].ID)
		require.Equal(t, i > 0, page.HasMore)
		cursor = page.NextCursor
	}
}
