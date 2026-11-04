package storage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEntitySuggestions(t *testing.T) {
	resetStorageState()
	t.Cleanup(resetStorageState)
	require.NoError(t, InitDB(filepath.Join(t.TempDir(), "suggestions.db")))
	conn, err := getDB()
	require.NoError(t, err)
	for _, row := range []struct {
		entity, label string
		bucket, total int64
	}{
		{"10.0.0.1", "Match-old", 1, 10},
		{"10.0.0.1", "Match-z", 2, 20},
		{"10.0.0.2", "Match-a", 1, 30},
		{"10.0.0.3", "Other", 1, 1000},
		{"10.0.0.4", "Literal%_\\name", 1, 1},
		{"10.0.0.5", "", 1, 2},
		{"10.0.0.6", "MÜNCHEN", 1, 3},
	} {
		_, err := conn.ExecContext(t.Context(), `
INSERT INTO metrics (granularity, dimension, bucket_start, entity, entity_label, total)
VALUES ('day', 'client', ?, ?, ?, ?)`, row.bucket, row.entity, row.label, row.total)
		require.NoError(t, err)
	}

	t.Run("filters after aggregation and ranks before limiting", func(t *testing.T) {
		rows, err := GetEntitySuggestions(t.Context(), DimensionClient, GranularityDay, 1, 2, "match", 2)
		require.NoError(t, err)
		require.Equal(t, []EntityMetric{
			{Entity: "10.0.0.2", Label: "Match-a", Total: 30},
			{Entity: "10.0.0.1", Label: "Match-z", Total: 30},
		}, rows)
		limited, err := GetEntitySuggestions(t.Context(), DimensionClient, GranularityDay, 1, 2, "match", 1)
		require.NoError(t, err)
		require.Equal(t, rows[:1], limited)
		oldLabel, err := GetEntitySuggestions(t.Context(), DimensionClient, GranularityDay, 1, 2, "old", 2)
		require.NoError(t, err)
		require.Empty(t, oldLabel)
	})
	t.Run("IP matching retains full counts and falls back to the IP", func(t *testing.T) {
		rows, err := GetEntitySuggestions(t.Context(), DimensionClient, GranularityDay, 1, 2, "10.0.0.1", 1)
		require.NoError(t, err)
		require.Equal(t, []EntityMetric{{Entity: "10.0.0.1", Label: "Match-z", Total: 30}}, rows)
		rows, err = GetEntitySuggestions(t.Context(), DimensionClient, GranularityDay, 1, 2, "10.0.0.5", 1)
		require.NoError(t, err)
		require.Equal(t, []EntityMetric{{Entity: "10.0.0.5", Label: "10.0.0.5", Total: 2}}, rows)
	})
	t.Run("wildcards are literal", func(t *testing.T) {
		rows, err := GetEntitySuggestions(t.Context(), DimensionClient, GranularityDay, 1, 2, "%_\\", 10)
		require.NoError(t, err)
		require.Equal(t, []EntityMetric{{Entity: "10.0.0.4", Label: "Literal%_\\name", Total: 1}}, rows)
	})
	t.Run("Unicode matching works on new pooled connections", func(t *testing.T) {
		held, err := conn.Conn(t.Context())
		require.NoError(t, err)
		defer held.Close()

		rows, err := GetEntitySuggestions(t.Context(), DimensionClient, GranularityDay, 1, 2, "münchen", 1)
		require.NoError(t, err)
		require.Equal(t, []EntityMetric{{Entity: "10.0.0.6", Label: "MÜNCHEN", Total: 3}}, rows)
	})
	t.Run("cancelled requests stop the query", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := GetEntitySuggestions(ctx, DimensionClient, GranularityDay, 1, 2, "match", 1)
		require.ErrorIs(t, err, context.Canceled)
	})
}
