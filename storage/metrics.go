package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// Metric dimensions.
const (
	DimensionGlobal    = "global"
	DimensionDomain    = "domain"
	DimensionClient    = "client"
	DimensionQueryType = "query_type"
)

// Metric granularities.
const (
	GranularityMinute  = "minute"
	GranularityHour    = "hour"
	GranularityDay     = "day"
	GranularityAllTime = "all_time"

	// GranularityWindow7d and GranularityWindow30d hold one row per entity
	// with the totals of the trailing window. The dashboard Top lists read
	// them instead of summing hundreds of thousands of day buckets.
	GranularityWindow7d  = "window_7d"
	GranularityWindow30d = "window_30d"
)

// Response categories used when an entry is folded into a metric bucket.
const (
	StatTotal     = "total"
	StatCached    = "cached"
	StatForwarded = "forwarded"
	StatBlocked   = "blocked"
	StatFiltered  = "filtered"
	StatLocal     = "local"
	StatError     = "error"
	StatOther     = "other"
	StatDropped   = "dropped"
)

// upsertMetricSQL adds a delta to an existing metric row or creates it. SQLite
// evaluates the whole statement inside the surrounding transaction, so the
// previous read-modify-write round trip disappears.
const upsertMetricSQL = `
INSERT INTO metrics (granularity, dimension, bucket_start, entity, entity_label,
                     total, blocked, cached, forwarded, filtered, local, other,
                     dropped, errors, total_latency_ms, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (granularity, dimension, bucket_start, entity) DO UPDATE SET
	total            = total + excluded.total,
	blocked          = blocked + excluded.blocked,
	cached           = cached + excluded.cached,
	forwarded        = forwarded + excluded.forwarded,
	filtered         = filtered + excluded.filtered,
	local            = local + excluded.local,
	other            = other + excluded.other,
	dropped          = dropped + excluded.dropped,
	errors           = errors + excluded.errors,
	total_latency_ms = total_latency_ms + excluded.total_latency_ms,
	entity_label     = CASE WHEN excluded.entity_label <> '' THEN excluded.entity_label
	                        ELSE entity_label END,
	updated_at       = excluded.updated_at`

// putMetricSQL writes a row with absolute values. Rollup rebuilds and fixtures
// use it instead of the delta upsert.
const putMetricSQL = `
INSERT INTO metrics (granularity, dimension, bucket_start, entity, entity_label,
                     total, blocked, cached, forwarded, filtered, local, other,
                     dropped, errors, total_latency_ms, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (granularity, dimension, bucket_start, entity) DO UPDATE SET
	entity_label     = excluded.entity_label,
	total            = excluded.total,
	blocked          = excluded.blocked,
	cached           = excluded.cached,
	forwarded        = excluded.forwarded,
	filtered         = excluded.filtered,
	local            = excluded.local,
	other            = excluded.other,
	dropped          = excluded.dropped,
	errors           = excluded.errors,
	total_latency_ms = excluded.total_latency_ms,
	updated_at       = excluded.updated_at`

// MetricRow is a persisted metrics row.
type MetricRow struct {
	ID           string
	Dimension    string
	Granularity  string
	BucketStart  int64
	Entity       string
	EntityLabel  string
	Total        uint64
	Blocked      uint64
	Cached       uint64
	Forwarded    uint64
	Filtered     uint64
	Local        uint64
	Other        uint64
	Dropped      uint64
	Errors       uint64
	TotalLatency uint64
	UpdatedAt    int64
}

// MetricsDelta is a set of counter increments for one metric bucket. Total is
// incremented once per query (including dropped and failed queries); errors and
// dropped are also exposed as dedicated counters.
type MetricsDelta struct {
	Total          uint64
	Blocked        uint64
	Cached         uint64
	Forwarded      uint64
	Filtered       uint64
	Local          uint64
	Other          uint64
	Dropped        uint64
	Errors         uint64
	TotalLatencyMs uint64
}

// Add merges other into d.
func (d *MetricsDelta) Add(other MetricsDelta) {
	d.Total += other.Total
	d.Blocked += other.Blocked
	d.Cached += other.Cached
	d.Forwarded += other.Forwarded
	d.Filtered += other.Filtered
	d.Local += other.Local
	d.Other += other.Other
	d.Dropped += other.Dropped
	d.Errors += other.Errors
	d.TotalLatencyMs += other.TotalLatencyMs
}

// MetricInput is one metric bucket increment to stage into a batch.
type MetricInput struct {
	Dimension   string
	Granularity string
	BucketStart int64
	Entity      string
	EntityLabel string
	Delta       MetricsDelta
}

// EntityMetric is the aggregated result for one entity over a window.
type EntityMetric struct {
	Entity  string
	Label   string
	Total   uint64
	Blocked uint64
}

// GlobalStats is the counter set for one global bucket.
type GlobalStats struct {
	Total          uint64
	Blocked        uint64
	Cached         uint64
	Forwarded      uint64
	Filtered       uint64
	Local          uint64
	Other          uint64
	Dropped        uint64
	Errors         uint64
	TotalLatencyMs uint64
}

// CacheHitRate returns cached / (cached + forwarded), matching the legacy
// statistics collector formula.
func (s GlobalStats) CacheHitRate() float64 {
	lookups := s.Cached + s.Forwarded
	if lookups == 0 {
		return 0
	}

	return float64(s.Cached) / float64(lookups)
}

// AverageResponseMs returns the mean response time over answered queries.
func (s GlobalStats) AverageResponseMs() uint64 {
	answered := s.Total - min(s.Dropped+s.Errors, s.Total)
	if answered == 0 {
		return 0
	}

	return s.TotalLatencyMs / answered
}

// StatCategory maps a stored response type to its metric category.
func StatCategory(responseType string) string {
	switch responseType {
	case "CACHED":
		return StatCached
	case "RESOLVED", "CONDITIONAL":
		return StatForwarded
	case "BLOCKED", "REBIND":
		return StatBlocked
	case "FILTERED", "NOTFQDN":
		return StatFiltered
	case "CUSTOMDNS", "HOSTSFILE", "SPECIAL", "SYNTHESIZED":
		return StatLocal
	case "BOGUS":
		return StatError
	default:
		return StatOther
	}
}

// ClientEntity returns the stable client metric key and display label. Named
// clients are keyed by their resolved name; unnamed clients fall back to IP.
func ClientEntity(ip, name string) (entity, label string) {
	label = strings.TrimSpace(name)
	if label == "" {
		label = ip
	}

	return label, label
}

// DayBucket returns the Unix timestamp of the start of t's local day.
func DayBucket(t time.Time) int64 {
	y, m, d := t.Date()

	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).Unix()
}

// BucketStart returns the metric bucket start for granularity.
func BucketStart(t time.Time, granularity string) int64 {
	switch granularity {
	case GranularityMinute:
		return t.Truncate(time.Minute).Unix()
	case GranularityHour:
		return t.Truncate(time.Hour).Unix()
	case GranularityDay:
		return DayBucket(t)
	default:
		return 0
	}
}

// UpsertMetrics stages every metric increment into batch. Buckets are
// accumulated by the query log writer before this call, so each metrics row is
// touched at most once per flush.
//
// Inputs that address the same metric bucket are merged first, because a batch
// may only contain one delta per primary key.
func UpsertMetrics(batch Batch, inputs []MetricInput) error {
	if len(inputs) == 0 {
		return nil
	}

	if batch.tx == nil {
		return errors.New("can't upsert metrics without a transaction")
	}

	merged, order, err := mergeMetricInputs(inputs)
	if err != nil {
		return err
	}

	stmt, err := batch.tx.PrepareContext(context.Background(), upsertMetricSQL)
	if err != nil {
		return fmt.Errorf("can't prepare %s upsert: %w", TableMetrics, err)
	}

	defer stmt.Close()

	now := updatedAt()

	for _, id := range order {
		input := merged[id]

		_, err := stmt.ExecContext(
			context.Background(),
			input.Granularity, input.Dimension, input.BucketStart, input.Entity, input.EntityLabel,
			input.Delta.Total, input.Delta.Blocked, input.Delta.Cached, input.Delta.Forwarded,
			input.Delta.Filtered, input.Delta.Local, input.Delta.Other, input.Delta.Dropped,
			input.Delta.Errors, input.Delta.TotalLatencyMs, now,
		)
		if err != nil {
			return fmt.Errorf("can't upsert %s metric %s: %w", TableMetrics, id, err)
		}
	}

	return nil
}

// mergeMetricInputs merges the inputs that address the same metric bucket and
// returns them in the order of their first occurrence.
func mergeMetricInputs(inputs []MetricInput) (map[string]*MetricInput, []string, error) {
	merged := make(map[string]*MetricInput, len(inputs))
	order := make([]string, 0, len(inputs))

	for _, input := range inputs {
		if input.Dimension == "" || input.Granularity == "" {
			return nil, nil, fmt.Errorf("invalid metric input: dimension=%q granularity=%q",
				input.Dimension, input.Granularity)
		}

		id := metricID(input.Dimension, input.Granularity, input.BucketStart, input.Entity)

		existing, ok := merged[id]
		if !ok {
			copied := input
			merged[id] = &copied
			order = append(order, id)

			continue
		}

		existing.Delta.Add(input.Delta)

		if input.EntityLabel != "" {
			existing.EntityLabel = input.EntityLabel
		}
	}

	return merged, order, nil
}

// putMetricRow writes one row with absolute values inside tx.
func putMetricRow(tx *sql.Tx, row *MetricRow) error {
	_, err := tx.ExecContext(context.Background(), putMetricSQL,
		row.Granularity, row.Dimension, row.BucketStart, row.Entity, row.EntityLabel,
		row.Total, row.Blocked, row.Cached, row.Forwarded, row.Filtered, row.Local,
		row.Other, row.Dropped, row.Errors, row.TotalLatency, row.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("can't write %s metric %s: %w", TableMetrics, row.ID, err)
	}

	return nil
}

// metricID builds the stable identifier of a metric bucket.
func metricID(dimension, granularity string, bucketStart int64, entity string) string {
	return fmt.Sprintf("%s:%s:%d:%s", dimension, granularity, bucketStart, entity)
}

const metricSelect = `
SELECT granularity, dimension, bucket_start, entity, entity_label,
       total, blocked, cached, forwarded, filtered, local, other, dropped, errors,
       total_latency_ms, updated_at
FROM metrics`

// scanMetricRows decodes every row of a metrics query.
func scanMetricRows(rows *sql.Rows) ([]*MetricRow, error) {
	defer rows.Close()

	result := make([]*MetricRow, 0)

	for rows.Next() {
		row, err := scanMetricRow(rows)
		if err != nil {
			return nil, err
		}

		result = append(result, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("can't read %s: %w", TableMetrics, err)
	}

	return result, nil
}

// metricScanner is implemented by *sql.Row and *sql.Rows.
type metricScanner interface {
	Scan(dest ...any) error
}

// scanMetricRow decodes one metrics row. SQLite stores integers as signed
// 64-bit values; the counters are converted back to uint64.
func scanMetricRow(scanner metricScanner) (*MetricRow, error) {
	var (
		row     MetricRow
		total   int64
		blocked int64
		cached  int64
		forward int64
		filter  int64
		local   int64
		other   int64
		dropped int64
		errs    int64
		latency int64
	)

	err := scanner.Scan(
		&row.Granularity, &row.Dimension, &row.BucketStart, &row.Entity, &row.EntityLabel,
		&total, &blocked, &cached, &forward, &filter, &local, &other, &dropped, &errs,
		&latency, &row.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	row.ID = metricID(row.Dimension, row.Granularity, row.BucketStart, row.Entity)
	row.Total = uint64(total)          //nolint:gosec // counters are non-negative
	row.Blocked = uint64(blocked)      //nolint:gosec // counters are non-negative
	row.Cached = uint64(cached)        //nolint:gosec // counters are non-negative
	row.Forwarded = uint64(forward)    //nolint:gosec // counters are non-negative
	row.Filtered = uint64(filter)      //nolint:gosec // counters are non-negative
	row.Local = uint64(local)          //nolint:gosec // counters are non-negative
	row.Other = uint64(other)          //nolint:gosec // counters are non-negative
	row.Dropped = uint64(dropped)      //nolint:gosec // counters are non-negative
	row.Errors = uint64(errs)          //nolint:gosec // counters are non-negative
	row.TotalLatency = uint64(latency) //nolint:gosec // counters are non-negative

	return &row, nil
}

// GetMetricSeries returns the stored rows of one dimension/granularity in the
// inclusive bucket range [start, end], ordered by bucket and entity. When
// entity is set only that entity is returned.
func GetMetricSeries(dimension, granularity, entity string, start, end int64) ([]*MetricRow, error) {
	if end < start {
		return nil, nil
	}

	conn, err := getDB()
	if err != nil {
		return nil, err
	}

	var (
		builder strings.Builder
		args    []any
	)

	builder.WriteString(metricSelect)
	builder.WriteString(`
WHERE granularity = ? AND dimension = ? AND bucket_start >= ? AND bucket_start <= ?`)
	args = append(args, granularity, dimension, start, end)

	if entity != "" {
		builder.WriteString(" AND entity = ?")
		args = append(args, entity)
	}

	builder.WriteString(" ORDER BY bucket_start, entity")

	rows, err := conn.QueryContext(context.Background(), builder.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("can't scan %s metrics: %w", TableMetrics, err)
	}

	result, err := scanMetricRows(rows)
	if err != nil {
		return nil, fmt.Errorf("can't scan %s metrics: %w", TableMetrics, err)
	}

	return result, nil
}

// GetGlobalStats returns the global counters for one bucket. A missing bucket
// returns zero values.
func GetGlobalStats(granularity string, bucketStart int64) (GlobalStats, error) {
	rows, err := GetMetricSeries(DimensionGlobal, granularity, "", bucketStart, bucketStart)
	if err != nil {
		return GlobalStats{}, err
	}

	if len(rows) == 0 {
		return GlobalStats{}, nil
	}

	row := rows[0]

	return GlobalStats{
		Total:          row.Total,
		Blocked:        row.Blocked,
		Cached:         row.Cached,
		Forwarded:      row.Forwarded,
		Filtered:       row.Filtered,
		Local:          row.Local,
		Other:          row.Other,
		Dropped:        row.Dropped,
		Errors:         row.Errors,
		TotalLatencyMs: row.TotalLatency,
	}, nil
}

// SumGlobalStats aggregates all global buckets in [start, end].
func SumGlobalStats(granularity string, start, end int64) (GlobalStats, error) {
	rows, err := GetMetricSeries(DimensionGlobal, granularity, "", start, end)
	if err != nil {
		return GlobalStats{}, err
	}

	var stats GlobalStats

	for _, row := range rows {
		stats.Total += row.Total
		stats.Blocked += row.Blocked
		stats.Cached += row.Cached
		stats.Forwarded += row.Forwarded
		stats.Filtered += row.Filtered
		stats.Local += row.Local
		stats.Other += row.Other
		stats.Dropped += row.Dropped
		stats.Errors += row.Errors
		stats.TotalLatencyMs += row.TotalLatency
	}

	return stats, nil
}

// GetTopEntities aggregates rows for one dimension/granularity across every
// entity in the inclusive bucket range. It never prunes entities by rank: the
// caller receives the complete source set and decides how to rank copies.
func GetTopEntities(dimension, granularity string, start, end int64) ([]EntityMetric, error) {
	conn, err := getDB()
	if err != nil {
		return nil, err
	}

	rows, err := conn.QueryContext(context.Background(), `
SELECT entity, MAX(entity_label), SUM(total), SUM(blocked)
FROM metrics
WHERE granularity = ? AND dimension = ?
  AND bucket_start >= ? AND bucket_start <= ?
  AND entity <> ''
GROUP BY entity
HAVING SUM(total) > 0 OR SUM(blocked) > 0
ORDER BY SUM(total) DESC, entity ASC`,
		granularity, dimension, start, end)
	if err != nil {
		return nil, fmt.Errorf("can't aggregate %s metrics: %w", TableMetrics, err)
	}

	defer rows.Close()

	result := make([]EntityMetric, 0)

	for rows.Next() {
		var (
			entity  string
			label   string
			total   int64
			blocked int64
		)

		if err := rows.Scan(&entity, &label, &total, &blocked); err != nil {
			return nil, fmt.Errorf("can't aggregate %s metrics: %w", TableMetrics, err)
		}

		result = append(result, EntityMetric{
			Entity:  entity,
			Label:   label,
			Total:   uint64(total),   //nolint:gosec // counters are non-negative
			Blocked: uint64(blocked), //nolint:gosec // counters are non-negative
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("can't aggregate %s metrics: %w", TableMetrics, err)
	}

	return result, nil
}

// GetQueryTypeCounts aggregates query_type counters for a granularity and
// inclusive bucket range.
func GetQueryTypeCounts(granularity string, start, end int64) (map[string]uint64, error) {
	entities, err := GetTopEntities(DimensionQueryType, granularity, start, end)
	if err != nil {
		return nil, err
	}

	counts := make(map[string]uint64, len(entities))

	for _, entity := range entities {
		counts[entity.Entity] += entity.Total
	}

	return counts, nil
}

// satInt64 clamps a counter to the int64 range.
func satInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}

	return int64(v)
}
