package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// Trailing window rollup definitions.
const (
	window7dDays  = 7
	window30dDays = 30

	// dimensionMeta holds internal bookkeeping rows (the rollup watermarks).
	// It is not a business dimension and is never ranked.
	dimensionMeta          = "meta"
	granularityWindowState = "window_state"

	// rollupRetentionMarginDays is how many extra days of day buckets the
	// retention cleanup keeps beyond the 30-day window, so the rollup always
	// has the bucket it needs when a day leaves the window.
	rollupRetentionMarginDays = 2
)

// WindowSpec describes one trailing window rollup.
type WindowSpec struct {
	Granularity string
	Days        int
}

// WindowSpecs returns the trailing window rollups that are maintained for the
// dashboard Top lists.
func WindowSpecs() []WindowSpec {
	return []WindowSpec{
		{Granularity: GranularityWindow7d, Days: window7dDays},
		{Granularity: GranularityWindow30d, Days: window30dDays},
	}
}

// WindowStart returns the first day bucket of the trailing window ending at
// now. A window of n days includes today and the n-1 days before it.
func WindowStart(now time.Time, days int) int64 {
	y, m, d := now.AddDate(0, 0, -(days - 1)).Date()

	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).Unix()
}

// InWindow reports whether ts falls into the trailing window ending at now.
func InWindow(ts, now time.Time, days int) bool {
	day := DayBucket(ts)

	return day >= WindowStart(now, days) && day <= DayBucket(now)
}

//nolint:gochecknoglobals // process-wide readiness flag for the rollup tables
var windowRollupsReady atomic.Bool

// WindowRollupsReady reports whether the window rollups cover the current
// window. Until the first successful maintenance run the dashboard falls back
// to the day buckets.
func WindowRollupsReady() bool {
	return windowRollupsReady.Load()
}

// rebuildRollupSQL folds the day buckets of the current window into one row per
// domain and client. bucket_start is 0 for every rollup row.
const rebuildRollupSQL = `
INSERT INTO metrics (granularity, dimension, bucket_start, entity, entity_label,
                     total, blocked, cached, forwarded, filtered, local, other,
                     dropped, errors, total_latency_ms, updated_at)
SELECT ?, dimension, 0, entity, MAX(entity_label),
       SUM(total), SUM(blocked), SUM(cached), SUM(forwarded), SUM(filtered),
       SUM(local), SUM(other), SUM(dropped), SUM(errors), SUM(total_latency_ms), ?
FROM metrics
WHERE granularity = 'day'
  AND dimension IN ('domain', 'client')
  AND bucket_start >= ? AND bucket_start <= ?
GROUP BY dimension, entity`

// subtractRollupSQL removes one expired day from every rollup row of one
// dimension. The scalar MAX clamps each counter at zero, matching the previous
// in-memory subtraction.
const subtractRollupSQL = `
UPDATE metrics AS w
SET total            = MAX(w.total - d.total, 0),
    blocked          = MAX(w.blocked - d.blocked, 0),
    cached           = MAX(w.cached - d.cached, 0),
    forwarded        = MAX(w.forwarded - d.forwarded, 0),
    filtered         = MAX(w.filtered - d.filtered, 0),
    local            = MAX(w.local - d.local, 0),
    other            = MAX(w.other - d.other, 0),
    dropped          = MAX(w.dropped - d.dropped, 0),
    errors           = MAX(w.errors - d.errors, 0),
    total_latency_ms = MAX(w.total_latency_ms - d.total_latency_ms, 0),
    updated_at       = ?
FROM (
	SELECT entity,
	       SUM(total) AS total, SUM(blocked) AS blocked, SUM(cached) AS cached,
	       SUM(forwarded) AS forwarded, SUM(filtered) AS filtered, SUM(local) AS local,
	       SUM(other) AS other, SUM(dropped) AS dropped, SUM(errors) AS errors,
	       SUM(total_latency_ms) AS total_latency_ms
	FROM metrics
	WHERE granularity = 'day' AND dimension = ? AND bucket_start = ? AND entity <> ''
	GROUP BY entity
) AS d
WHERE w.granularity = ? AND w.dimension = ? AND w.bucket_start = 0 AND w.entity = d.entity`

// deleteZeroRollupSQL drops rollup rows that no longer carry any counter, so
// entities that left the window do not linger.
const deleteZeroRollupSQL = `
DELETE FROM metrics
WHERE granularity = ?
  AND total = 0 AND blocked = 0 AND cached = 0 AND forwarded = 0 AND filtered = 0
  AND local = 0 AND other = 0 AND dropped = 0 AND errors = 0 AND total_latency_ms = 0`

// MaintainWindowRollups keeps the trailing window rollups in sync with the day
// buckets. Each rollup row is one entity and holds the totals of the whole
// window; the dashboard Top lists read these rows instead of every day bucket.
//
// The rollups are maintained incrementally: the writer adds each query to the
// rollup of both windows, and this function subtracts the days that fall out of
// the window. On the first run (fresh database, or an upgrade from a schema
// without rollups) the rollup is rebuilt from the day buckets once.
//
// It is idempotent and cheap when no day boundary has been crossed.
func MaintainWindowRollups(now time.Time) error {
	if _, err := getDB(); err != nil {
		return err
	}

	plan, err := planWindowRollups(now)
	if err != nil {
		return err
	}

	// A rebuild replaces the whole window from the day buckets, so the writer
	// must not add deltas while it runs.
	if plan.needsRebuild() {
		windowRollupsReady.Store(false)
	}

	if err := plan.apply(now); err != nil {
		return err
	}

	for _, spec := range plan.specs {
		if err := deleteZeroRollupRows(spec.Granularity); err != nil {
			return err
		}
	}

	windowRollupsReady.Store(true)

	return nil
}

// rollupPlan describes what each window needs: the day it should start at, the
// day it currently covers and whether it must be rebuilt from the day buckets.
type rollupPlan struct {
	specs   []WindowSpec
	starts  []int64
	storeds []int64
	rebuild []bool
}

// planWindowRollups reads the stored watermarks and computes the desired state
// of every window.
func planWindowRollups(now time.Time) (*rollupPlan, error) {
	specs := WindowSpecs()

	plan := &rollupPlan{
		specs:   specs,
		starts:  make([]int64, len(specs)),
		storeds: make([]int64, len(specs)),
		rebuild: make([]bool, len(specs)),
	}

	for i, spec := range specs {
		start := WindowStart(now, spec.Days)

		stored, found, err := readWindowStart(spec.Granularity)
		if err != nil {
			return nil, err
		}

		plan.starts[i] = start
		plan.storeds[i] = stored
		// No watermark yet (first run or an upgrade), a zeroed watermark or
		// one ahead of the window (clock moved back) all need a rebuild.
		plan.rebuild[i] = !found || stored <= 0 || stored > start
	}

	return plan, nil
}

// needsRebuild reports whether any window must be recomputed from scratch.
func (p *rollupPlan) needsRebuild() bool {
	for _, rebuild := range p.rebuild {
		if rebuild {
			return true
		}
	}

	return false
}

// apply rebuilds or trims each window and persists the new watermarks.
func (p *rollupPlan) apply(now time.Time) error {
	for i, spec := range p.specs {
		if p.rebuild[i] {
			if err := rebuildWindowRollup(spec, p.starts[i], DayBucket(now)); err != nil {
				return err
			}

			continue
		}

		for day := p.storeds[i]; day < p.starts[i]; day = nextDayBucket(day) {
			if err := subtractDayFromRollup(spec, day); err != nil {
				return err
			}
		}

		if p.storeds[i] != p.starts[i] {
			if err := writeWindowStart(spec.Granularity, p.starts[i]); err != nil {
				return err
			}
		}
	}

	return nil
}

// nextDayBucket returns the day bucket that follows day.
func nextDayBucket(day int64) int64 {
	return DayBucket(time.Unix(day, 0).AddDate(0, 0, 1))
}

// windowStateID is the metric identifier of one rollup watermark row.
func windowStateID(granularity string) string {
	return metricID(dimensionMeta, granularityWindowState, 0, granularity)
}

// readWindowStart reads the first day bucket the rollup currently covers.
func readWindowStart(granularity string) (int64, bool, error) {
	conn, err := getDB()
	if err != nil {
		return 0, false, err
	}

	var total int64

	err = conn.QueryRowContext(context.Background(), `
SELECT total FROM metrics
WHERE granularity = ? AND dimension = ? AND bucket_start = 0 AND entity = ?`,
		granularityWindowState, dimensionMeta, granularity).Scan(&total)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}

	if err != nil {
		return 0, false, fmt.Errorf("can't read %s watermark: %w", granularity, err)
	}

	return total, true, nil
}

// writeWindowStart persists the rollup watermark.
func writeWindowStart(granularity string, start int64) error {
	return withTx(func(tx *sql.Tx) error {
		return writeWindowStartTx(tx, granularity, start)
	})
}

// writeWindowStartTx persists the rollup watermark inside tx.
func writeWindowStartTx(tx *sql.Tx, granularity string, start int64) error {
	return putMetricRow(tx, &MetricRow{
		ID:          windowStateID(granularity),
		Dimension:   dimensionMeta,
		Granularity: granularityWindowState,
		Entity:      granularity,
		Total:       uint64(start), //nolint:gosec // day buckets are positive
		UpdatedAt:   updatedAt(),
	})
}

// rebuildWindowRollup recomputes the rollup of one window from the day buckets
// and replaces the stored rows. It is used on the first run and after the
// watermark was lost.
func rebuildWindowRollup(spec WindowSpec, start, end int64) error {
	return withTx(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(context.Background(),
			`DELETE FROM metrics WHERE granularity = ?`, spec.Granularity)
		if err != nil {
			return fmt.Errorf("can't clear %s metrics: %w", spec.Granularity, err)
		}

		_, err = tx.ExecContext(context.Background(), rebuildRollupSQL,
			spec.Granularity, updatedAt(), start, end)
		if err != nil {
			return fmt.Errorf("can't rebuild %s metrics: %w", spec.Granularity, err)
		}

		return writeWindowStartTx(tx, spec.Granularity, start)
	})
}

// subtractDayFromRollup removes the day that just left the window from both
// entity dimensions.
func subtractDayFromRollup(spec WindowSpec, day int64) error {
	return withTx(func(tx *sql.Tx) error {
		for _, dimension := range []string{DimensionDomain, DimensionClient} {
			_, err := tx.ExecContext(context.Background(), subtractRollupSQL,
				updatedAt(), dimension, day, spec.Granularity, dimension)
			if err != nil {
				return fmt.Errorf("can't expire %s day %d from %s: %w",
					dimension, day, spec.Granularity, err)
			}
		}

		return nil
	})
}

// deleteZeroRollupRows drops the rollup rows that no longer carry a counter.
func deleteZeroRollupRows(granularity string) error {
	conn, err := getDB()
	if err != nil {
		return err
	}

	if _, err := conn.ExecContext(context.Background(), deleteZeroRollupSQL, granularity); err != nil {
		return fmt.Errorf("can't prune %s metrics: %w", granularity, err)
	}

	return nil
}
