package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Supported retention windows in months.
const (
	minRetentionMonths = 1
	maxRetentionMonths = 12
)

// QueryLogEntry is a single parsed query log record destined for the query_logs table.
type QueryLogEntry struct {
	Timestamp    int64 // Unix milliseconds
	ClientIP     string
	ClientName   string
	Domain       string
	QueryType    string
	ResponseType string
	Reason       string
	DurationMs   uint16
}

// QueryLogReadEntry is a row from the query_logs table for read operations.
type QueryLogReadEntry struct {
	ID           int
	Timestamp    int64
	ClientIP     string
	ClientName   string
	Domain       string
	QueryType    string
	ResponseType string
	Reason       string
	DurationMs   uint16
}

// QueryLogPage is a single page of query log entries ordered newest first.
// NextCursor is opaque to the caller and only set when HasMore is true.
type QueryLogPage struct {
	Entries    []*QueryLogReadEntry
	NextCursor string
	HasMore    bool
}

// CleanupResult reports how many rows a normal cleanup removed.
type CleanupResult struct {
	QueryLogsDeleted int
	MetricsDeleted   int
}

const insertQueryLogSQL = `
INSERT INTO query_logs (timestamp, client_ip, client_name, domain, query_type,
                        response_type, reason, duration_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

// BatchInsertQueryLogs bulk-inserts all provided entries into the query_logs
// table. When a batch is provided the insert is staged into it and not
// committed (the caller commits); otherwise the insert is committed
// immediately.
func BatchInsertQueryLogs(entries []*QueryLogEntry, batchs ...Batch) error {
	if len(entries) == 0 {
		return nil
	}

	insert := func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(context.Background(), insertQueryLogSQL)
		if err != nil {
			return fmt.Errorf("can't prepare %s insert: %w", TableQueryLogs, err)
		}

		defer stmt.Close()

		for _, entry := range entries {
			_, err := stmt.ExecContext(
				context.Background(),
				entry.Timestamp, entry.ClientIP, entry.ClientName, entry.Domain,
				entry.QueryType, entry.ResponseType, entry.Reason, entry.DurationMs,
			)
			if err != nil {
				return fmt.Errorf("can't insert into %s: %w", TableQueryLogs, err)
			}
		}

		return nil
	}

	if len(batchs) > 0 && batchs[0].tx != nil {
		return insert(batchs[0].tx)
	}

	return withTx(insert)
}

// queryLogFilter holds the optional filters for GetQueryLogsPage.
type queryLogFilter struct {
	search       string
	responseType string
	client       string
	questionType string
}

// queryLogsPageQuery builds the keyset page statement. Filters are applied to
// the newest rows first, so a page never needs an offset scan.
func queryLogsPageQuery(cursor string, limit int, filter queryLogFilter) (string, []any, error) {
	var (
		builder strings.Builder
		args    []any
	)

	builder.WriteString(`
SELECT id, timestamp, client_ip, client_name, domain, query_type, response_type, reason, duration_ms
FROM query_logs
WHERE 1 = 1`)

	if cursor != "" {
		pivotID, err := parseQueryLogsCursor(cursor)
		if err != nil {
			return "", nil, err
		}

		builder.WriteString(" AND id < ?")
		args = append(args, pivotID)
	}

	if filter.search != "" {
		builder.WriteString(` AND domain LIKE ? ESCAPE '\'`)
		args = append(args, likePattern(filter.search))
	}

	if filter.responseType != "" {
		builder.WriteString(" AND response_type = ?")
		args = append(args, filter.responseType)
	}

	if filter.client != "" {
		builder.WriteString(` AND (client_ip LIKE ? ESCAPE '\' OR client_name LIKE ? ESCAPE '\')`)
		pattern := likePattern(filter.client)
		args = append(args, pattern, pattern)
	}

	if filter.questionType != "" {
		builder.WriteString(" AND query_type = ?")
		args = append(args, filter.questionType)
	}

	builder.WriteString(" ORDER BY id DESC LIMIT ?")
	args = append(args, limit+1)

	return builder.String(), args, nil
}

// GetQueryLogsPage returns a page of query log entries ordered newest first,
// using keyset pagination over the auto-increment primary key. A cursor is the
// string form of the last entry's id; the next page starts strictly below it.
// An empty cursor starts at the newest entry.
//
// The client filter matches either the resolved name or the raw IP, so
// suggestions and plain IP searches both work.
func GetQueryLogsPage(
	cursor string, limit int,
	search, responseType, client, questionType string,
) (*QueryLogPage, error) {
	if limit <= 0 {
		limit = 100
	}

	conn, err := getDB()
	if err != nil {
		return nil, err
	}

	query, args, err := queryLogsPageQuery(cursor, limit, queryLogFilter{
		search:       search,
		responseType: responseType,
		client:       client,
		questionType: questionType,
	})
	if err != nil {
		return nil, err
	}

	rows, err := conn.QueryContext(context.Background(), query, args...)
	if err != nil {
		return nil, fmt.Errorf("can't read %s: %w", TableQueryLogs, err)
	}

	collected, err := scanQueryLogRows(rows)
	if err != nil {
		return nil, err
	}

	page := &QueryLogPage{Entries: collected}

	if len(collected) > limit {
		page.Entries = collected[:limit]
		page.HasMore = true
		page.NextCursor = formatQueryLogsCursor(page.Entries[limit-1])
	}

	return page, nil
}

// scanQueryLogRows decodes every row of a query_logs query.
func scanQueryLogRows(rows *sql.Rows) ([]*QueryLogReadEntry, error) {
	defer rows.Close()

	collected := make([]*QueryLogReadEntry, 0)

	for rows.Next() {
		var (
			entry      QueryLogReadEntry
			durationMs int64
		)

		err := rows.Scan(
			&entry.ID, &entry.Timestamp, &entry.ClientIP, &entry.ClientName, &entry.Domain,
			&entry.QueryType, &entry.ResponseType, &entry.Reason, &durationMs,
		)
		if err != nil {
			return nil, fmt.Errorf("can't read %s: %w", TableQueryLogs, err)
		}

		entry.DurationMs = uint16(min(durationMs, int64(^uint16(0)))) //nolint:gosec // clamped above

		collected = append(collected, &entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("can't read %s: %w", TableQueryLogs, err)
	}

	return collected, nil
}

// GetQueryLogsTotal returns the all-time number of queries recorded in the
// all_time global metric. It is an O(1) lookup.
func GetQueryLogsTotal() (int64, error) {
	stats, err := GetGlobalStats(GranularityAllTime, 0)
	if err != nil {
		return 0, err
	}

	return satInt64(stats.Total), nil
}

func parseQueryLogsCursor(cursor string) (int, error) {
	id, err := strconv.Atoi(cursor)
	if err != nil {
		return 0, fmt.Errorf("invalid query logs cursor %q: %w", cursor, err)
	}

	return id, nil
}

func formatQueryLogsCursor(entry *QueryLogReadEntry) string {
	return strconv.Itoa(entry.ID)
}

// likePattern builds a substring LIKE pattern and escapes the wildcards that
// may appear in user input.
func likePattern(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

	return "%" + replacer.Replace(value) + "%"
}

// CleanUp deletes query logs and short-lived metrics older than the requested
// number of months (1-12, default 1). Global daily metrics and all_time counters
// are always preserved, so long-term totals survive cleanup.
func CleanUp(months int) (CleanupResult, error) {
	var result CleanupResult

	cutoff := time.Now().AddDate(0, -clampRetentionMonths(months), 0)

	deleted, err := deleteExpiredQueryLogs(cutoff)
	if err != nil {
		return result, err
	}

	result.QueryLogsDeleted = deleted

	deleted, err = deleteExpiredMetrics(cutoff)
	if err != nil {
		return result, err
	}

	result.MetricsDeleted = deleted

	return result, nil
}

// clampRetentionMonths limits the retention window to the supported range.
func clampRetentionMonths(months int) int {
	if months < minRetentionMonths {
		return minRetentionMonths
	}

	if months > maxRetentionMonths {
		return maxRetentionMonths
	}

	return months
}

// deleteExpiredQueryLogs deletes the query log rows older than the cutoff.
func deleteExpiredQueryLogs(cutoff time.Time) (int, error) {
	conn, err := getDB()
	if err != nil {
		return 0, err
	}

	result, err := conn.ExecContext(context.Background(), "DELETE FROM query_logs WHERE timestamp < ?", cutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("can't delete expired query_logs: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("can't count expired query_logs: %w", err)
	}

	return int(deleted), nil
}

// deleteExpiredMetrics deletes the short-lived metric buckets older than the
// cutoff. The global daily metrics and the all_time counters are kept.
func deleteExpiredMetrics(cutoff time.Time) (int, error) {
	conn, err := getDB()
	if err != nil {
		return 0, err
	}

	cutoffSec := cutoff.Unix()
	cutoffDay := DayBucket(cutoff)

	// The trailing 30-day rollup subtracts each day that leaves its window
	// from the day bucket of that day. Keep a safety margin of whole days so a
	// cleanup can never remove a day bucket before the rollup has expired it.
	rollupCutoff := DayBucket(
		time.Now().AddDate(0, 0, -(window30dDays + rollupRetentionMarginDays)),
	)
	if rollupCutoff < cutoffDay {
		cutoffDay = rollupCutoff
	}

	total := 0

	// Delete one dimension at a time so every statement is a contiguous
	// primary-key range scan (granularity, dimension, bucket_start).
	for _, granularity := range []string{GranularityMinute, GranularityHour} {
		for _, dimension := range cleanupDimensions {
			deleted, err := deleteMetricBuckets(conn, granularity, dimension, cutoffSec)
			if err != nil {
				return total, err
			}

			total += deleted
		}
	}

	for _, dimension := range cleanupDimensions {
		if dimension == DimensionGlobal {
			// Global daily metrics are the long-term history and are kept.
			continue
		}

		deleted, err := deleteMetricBuckets(conn, GranularityDay, dimension, cutoffDay)
		if err != nil {
			return total, err
		}

		total += deleted
	}

	return total, nil
}

// cleanupDimensions lists every dimension the writer creates, so cleanup can
// address each primary-key prefix without scanning the whole granularity.
//
//nolint:gochecknoglobals // immutable list
var cleanupDimensions = []string{
	DimensionGlobal,
	DimensionDomain,
	DimensionClient,
	DimensionQueryType,
}

// deleteMetricBuckets removes every bucket of one dimension older than cutoff.
func deleteMetricBuckets(conn *sql.DB, granularity, dimension string, cutoff int64) (int, error) {
	result, err := conn.ExecContext(context.Background(), `
DELETE FROM metrics
WHERE granularity = ? AND dimension = ? AND bucket_start < ?`,
		granularity, dimension, cutoff)
	if err != nil {
		return 0, fmt.Errorf("can't delete expired %s metrics: %w", granularity, err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("can't count expired %s metrics: %w", granularity, err)
	}

	return int(deleted), nil
}
