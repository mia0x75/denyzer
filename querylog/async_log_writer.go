package querylog

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/storage"
	"github.com/mia0x75/denyzer/util"
)

const (
	loggerPrefixAsyncLogWriter = "asyncLogWriter"
	defaultChanCap             = 2000
	defaultBatchSize           = 100
	defaultFlushInterval       = 5 * time.Second
	ttlCleanupInterval         = 12 * time.Hour
	windowMaintenanceInterval  = time.Hour
)

const maxDurationMs = ^uint16(0)

// AsyncLogWriter is a high-performance, non-blocking query log writer backed by
// the embedded SQLite store. It uses an unbuffered channel as a ring buffer,
// dual-trigger flushing (batch size OR timeout), and in-memory pre-aggregation
// to reduce the number of DB operations per flush cycle.
type AsyncLogWriter struct {
	ch            chan *storage.QueryLogEntry
	flushInterval time.Duration
	batchSize     int
	retention     config.QueryLogRetention
	done          chan struct{}
	closed        atomic.Bool
	dropped       atomic.Uint64
	failed        atomic.Uint64
}

// NewAsyncLogWriter creates and starts an AsyncLogWriter. The underlying SQLite
// database is opened (or reused) and two goroutines are launched: a consumer
// that flushes buffered entries on dual-trigger, and a TTL cleanup worker.
func NewAsyncLogWriter(ctx context.Context, dbPath string, retention config.QueryLogRetention,
	flushInterval time.Duration, batchSize int,
) (*AsyncLogWriter, error) {
	if err := storage.InitDB(dbPath); err != nil {
		return nil, err
	}

	if flushInterval <= 0 {
		flushInterval = defaultFlushInterval
	}

	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}

	w := &AsyncLogWriter{
		ch:            make(chan *storage.QueryLogEntry, defaultChanCap),
		flushInterval: flushInterval,
		batchSize:     batchSize,
		retention:     retention,
		done:          make(chan struct{}),
	}

	go w.consume(ctx)
	go w.ttlCleanup(ctx)
	go w.maintainWindows(ctx)

	return w, nil
}

// Write accepts a log entry and sends it to the channel in a non-blocking
// fashion. If the channel is full, the entry is dropped (same semantics as the
// old QueryLoggingResolver).
func (w *AsyncLogWriter) Write(entry *LogEntry) {
	if w.closed.Load() {
		return
	}

	e := &storage.QueryLogEntry{
		Timestamp:    entry.Start.UnixMilli(),
		ClientIP:     entry.ClientIP,
		ClientName:   strings.Join(entry.ClientNames, "; "),
		Domain:       util.ExtractDomainOnly(entry.QuestionName),
		QueryType:    entry.QuestionType,
		ResponseType: entry.ResponseType,
		Reason:       entry.ResponseReason,
		DurationMs:   uint16(min(entry.DurationMs, int64(maxDurationMs))), //nolint:gosec // clamped
	}

	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
		log.PrefixedLog(loggerPrefixAsyncLogWriter).Warn("async writer channel full, dropping entry")
	}
}

// RecordError counts a query that failed before it could be turned into a log
// entry. The counter is folded into the next flush so it appears in the
// statistics as a failed query.
func (w *AsyncLogWriter) RecordError() {
	if w.closed.Load() {
		return
	}

	w.failed.Add(1)
}

// CleanUp is a no-op; TTL cleanup is handled by the background goroutine.
func (w *AsyncLogWriter) CleanUp() {}

// Close signals the consumer goroutine to drain remaining entries and blocks
// until it is done. Safe to call multiple times.
func (w *AsyncLogWriter) Close() error {
	if w.closed.Swap(true) {
		return nil
	}

	// The consumer goroutine exits its drain loop when the channel is closed
	// and the buffer is empty, then closes w.done.
	close(w.ch)
	<-w.done

	return nil
}

// consume implements the dual-trigger flush loop: entries are buffered in a
// slice and flushed when either batchSize is reached or the flushInterval
// timer fires. On context cancellation it drains the channel.
func (w *AsyncLogWriter) consume(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(w.flushInterval)
	defer ticker.Stop()

	buf := make([]*storage.QueryLogEntry, 0, w.batchSize)
	agg := newAggregationMaps()

	for {
		select {
		case entry, ok := <-w.ch:
			if !ok {
				// Channel closed — final drain.
				w.flush(buf, agg)

				return
			}

			buf = append(buf, entry)

			if len(buf) >= w.batchSize {
				w.flush(buf, agg)
				buf = buf[:0]
				agg.reset()
				ticker.Reset(w.flushInterval)
			}

		case <-ticker.C:
			w.flush(buf, agg)
			buf = buf[:0]
			agg.reset()

		case <-ctx.Done():
			w.drain(buf, agg)

			return
		}
	}
}

// drain consumes whatever is left in the channel after cancellation and flushes it.
func (w *AsyncLogWriter) drain(buf []*storage.QueryLogEntry, agg *aggregationMaps) {
	for {
		select {
		case entry, ok := <-w.ch:
			if !ok {
				w.flush(buf, agg)

				return
			}

			buf = append(buf, entry)
		default:
			w.flush(buf, agg)

			return
		}
	}
}

// metricKey identifies one in-memory metric bucket.
type metricKey struct {
	dimension   string
	granularity string
	bucketStart int64
	entity      string
	label       string
}

// aggregationMaps holds the reusable in-memory pre-aggregation buckets. Every
// bucket is a storage.MetricsDelta, keyed by dimension, granularity, bucket and
// entity. A flush stages one row per key and commits logs + metrics together.
type aggregationMaps struct {
	metrics map[metricKey]*storage.MetricsDelta
}

func newAggregationMaps() *aggregationMaps {
	return &aggregationMaps{
		metrics: make(map[metricKey]*storage.MetricsDelta),
	}
}

func (a *aggregationMaps) reset() {
	clear(a.metrics)
}

// flush pre-aggregates the buffer in memory and writes all rows and metrics
// for the flush cycle into one storage batch, committing once so the cycle is
// atomic. Entries dropped because the channel was full and resolver failures
// recorded since the last flush are folded into the current bucket.
func (w *AsyncLogWriter) flush(buf []*storage.QueryLogEntry, agg *aggregationMaps) {
	logger := log.PrefixedLog(loggerPrefixAsyncLogWriter)

	dropped := w.dropped.Swap(0)
	failed := w.failed.Swap(0)

	if len(buf) == 0 && dropped == 0 && failed == 0 {
		return
	}

	agg.aggregate(buf)
	agg.addFailures(dropped, failed)

	batch, err := storage.NewBatch()
	if err != nil {
		logger.Errorf("can't create storage batch, dropping %d query log entries: %v", len(buf), err)

		return
	}

	committed := false

	defer func() {
		if !committed {
			storage.RollbackBatch(batch)
		}
	}()

	if err := storage.BatchInsertQueryLogs(buf, batch); err != nil {
		logger.Errorf("batch insert query_logs failed: %v", err)

		return
	}

	if err := agg.persist(batch); err != nil {
		logger.Errorf("can't stage metric updates: %v", err)

		return
	}

	if err := storage.CommitBatch(batch); err != nil {
		logger.Errorf("can't commit query log and metric batch: %v", err)

		return
	}

	committed = true
}

// addWindowDeltas folds one entry into the trailing window rollups. It is a
// no-op until the rollups have been built from the day buckets.
func (a *aggregationMaps) addWindowDeltas(
	entry *storage.QueryLogEntry, ts time.Time, entity, label, category string,
) {
	if !storage.WindowRollupsReady() {
		return
	}

	now := time.Now()

	for _, spec := range storage.WindowSpecs() {
		if !storage.InWindow(ts, now, spec.Days) {
			continue
		}

		if entry.Domain != "" {
			a.add(storage.DimensionDomain, spec.Granularity, 0, entry.Domain, entry.Domain,
				category, entry.DurationMs)
		}

		if entity != "" {
			a.add(storage.DimensionClient, spec.Granularity, 0, entity, label, category, entry.DurationMs)
		}
	}
}

// aggregate accumulates all entries of a batch into the in-memory counters.
func (a *aggregationMaps) aggregate(entries []*storage.QueryLogEntry) {
	for _, entry := range entries {
		a.addEntry(entry)
	}
}

func (a *aggregationMaps) addEntry(entry *storage.QueryLogEntry) {
	ts := time.UnixMilli(entry.Timestamp)
	category := storage.StatCategory(entry.ResponseType)
	blocked := category == storage.StatBlocked

	entity, label := storage.ClientEntity(entry.ClientIP, entry.ClientName)

	for _, granularity := range []string{
		storage.GranularityMinute,
		storage.GranularityHour,
		storage.GranularityDay,
	} {
		bucket := storage.BucketStart(ts, granularity)

		a.add(storage.DimensionGlobal, granularity, bucket, "", "", category, entry.DurationMs)

		if entry.Domain != "" {
			a.add(storage.DimensionDomain, granularity, bucket, entry.Domain, entry.Domain,
				category, entry.DurationMs)
		}

		if entity != "" {
			a.add(storage.DimensionClient, granularity, bucket, entity, label, category, entry.DurationMs)
		}
	}

	// Trailing window rollups: one row per entity and window instead of one
	// row per entity and bucket. Only entries inside the window are folded in;
	// the maintenance job subtracts the days that leave the window.
	//
	// While the rollups are being rebuilt they are skipped: the rebuild reads
	// the day buckets, which already contain every committed entry.
	a.addWindowDeltas(entry, ts, entity, label, category)

	a.add(storage.DimensionGlobal, storage.GranularityAllTime, 0,
		"", "", category, entry.DurationMs)

	qtype := entry.QueryType
	if qtype == "" {
		qtype = "UNKNOWN"
	}

	qtypeDelta := &storage.MetricsDelta{
		Total:          1,
		TotalLatencyMs: uint64(entry.DurationMs),
	}
	if blocked {
		qtypeDelta.Blocked = 1
	}

	a.addDelta(storage.DimensionQueryType, storage.GranularityDay,
		storage.DayBucket(ts), qtype, qtype, qtypeDelta)
	a.addDelta(storage.DimensionQueryType, storage.GranularityAllTime,
		0, qtype, qtype, qtypeDelta)
}

func (a *aggregationMaps) add(
	dimension, granularity string, bucketStart int64, entity, label, category string, durationMs uint16,
) {
	delta := &storage.MetricsDelta{
		Total:          1,
		TotalLatencyMs: uint64(durationMs),
	}

	switch category {
	case storage.StatCached:
		delta.Cached = 1
	case storage.StatForwarded:
		delta.Forwarded = 1
	case storage.StatBlocked:
		delta.Blocked = 1
	case storage.StatFiltered:
		delta.Filtered = 1
	case storage.StatLocal:
		delta.Local = 1
	case storage.StatError:
		delta.Errors = 1
	default:
		delta.Other = 1
	}

	a.addDelta(dimension, granularity, bucketStart, entity, label, delta)
}

func (a *aggregationMaps) addDelta(
	dimension, granularity string, bucketStart int64, entity, label string, delta *storage.MetricsDelta,
) {
	key := metricKey{
		dimension:   dimension,
		granularity: granularity,
		bucketStart: bucketStart,
		entity:      entity,
		label:       label,
	}

	existing, ok := a.metrics[key]
	if !ok {
		existing = &storage.MetricsDelta{}
		a.metrics[key] = existing
	}

	existing.Add(*delta)
}

// addFailures folds dropped entries and resolver failures into the current
// global time buckets. They carry no latency and are counted exactly once in
// the total.
func (a *aggregationMaps) addFailures(dropped, failed uint64) {
	if dropped == 0 && failed == 0 {
		return
	}

	now := time.Now()

	for _, granularity := range []string{
		storage.GranularityMinute,
		storage.GranularityHour,
		storage.GranularityDay,
		storage.GranularityAllTime,
	} {
		bucket := storage.BucketStart(now, granularity)
		if granularity == storage.GranularityAllTime {
			bucket = 0
		}

		a.addDelta(storage.DimensionGlobal, granularity, bucket, "", "", &storage.MetricsDelta{
			Total:   dropped + failed,
			Dropped: dropped,
			Errors:  failed,
		})
	}
}

// persist stages every metric upsert into batch.
func (a *aggregationMaps) persist(batch storage.Batch) error {
	if len(a.metrics) == 0 {
		return nil
	}

	inputs := make([]storage.MetricInput, 0, len(a.metrics))

	for key, delta := range a.metrics {
		inputs = append(inputs, storage.MetricInput{
			Dimension:   key.dimension,
			Granularity: key.granularity,
			BucketStart: key.bucketStart,
			Entity:      key.entity,
			EntityLabel: key.label,
			Delta:       *delta,
		})
	}

	if err := storage.UpsertMetrics(batch, inputs); err != nil {
		return fmt.Errorf("persist metrics: %w", err)
	}

	return nil
}

// ttlCleanup runs a periodic TTL cleanup across all tables.
func (w *AsyncLogWriter) ttlCleanup(ctx context.Context) {
	ticker := time.NewTicker(ttlCleanupInterval)
	defer ticker.Stop()

	logger := log.PrefixedLog(loggerPrefixAsyncLogWriter)

	for {
		select {
		case <-ticker.C:
			logger.Debug("running history cleanup")

			if _, err := storage.CleanUp(w.retention.EffectiveHistoryMonths()); err != nil {
				logger.Errorf("history cleanup failed: %v", err)
			}

		case <-ctx.Done():
			return
		}
	}
}

// maintainWindows keeps the trailing window rollups in sync. The first run
// rebuilds them from the day buckets (an upgrade), later runs only subtract the
// days that leave the window.
func (w *AsyncLogWriter) maintainWindows(ctx context.Context) {
	logger := log.PrefixedLog(loggerPrefixAsyncLogWriter)

	maintain := func() {
		if err := storage.MaintainWindowRollups(time.Now()); err != nil {
			logger.Errorf("window rollup maintenance failed: %v", err)
		}
	}

	maintain()

	ticker := time.NewTicker(windowMaintenanceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			maintain()

		case <-ctx.Done():
			return
		}
	}
}
