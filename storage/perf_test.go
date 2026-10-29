package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The dashboard performance fixtures simulate a deployment that has recorded a
// large number of queries. The default scale is one million queries spread over
// the 30-day retention window; DENYZER_PERF_QUERIES overrides it.
const (
	perfDefaultQueries = 1_000_000
	perfUniqueDomains  = 50_000
	perfUniqueClients  = 500
	perfWindowDays     = 30
	perfSeedBatchSize  = 5_000
	perfBlockedPercent = 10
)

type perfFixture struct {
	path string
	// domainRows and clientRows are the number of metric rows seeded per
	// dimension and granularity, reported for context.
	domainDayRows   int
	clientDayRows   int
	clientHourRows  int
	clientMinRows   int
	domainHourRows  int
	domainMinRows   int
	seededAt        time.Time
	seedDuration    time.Duration
	queryCount      int
	totalMetricRows int
}

func perfQueryCount(tb testing.TB) int {
	tb.Helper()

	raw := os.Getenv("DENYZER_PERF_QUERIES")
	if raw == "" {
		return perfDefaultQueries
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		tb.Fatalf("invalid DENYZER_PERF_QUERIES %q", raw)
	}

	return n
}

var (
	perfOnce       sync.Once
	perfCached     perfFixture
	perfRollupOnce sync.Once
	perfRollupCost time.Duration
)

// perfFixtureFor returns the process-wide million-row fixture, seeding it on
// the first call. All benchmarks share it so the (slow) seed runs once.
func perfFixtureFor(tb testing.TB) perfFixture {
	tb.Helper()

	perfOnce.Do(func() {
		perfCached = seedPerfFixture(tb)
	})

	return perfCached
}

// perfRollupsFor builds the trailing window rollups from the seeded day buckets
// once per process and returns how long the bootstrap took. It mirrors what
// MaintainWindowRollups does on the first run after an upgrade.
func perfRollupsFor(tb testing.TB) time.Duration {
	tb.Helper()

	fixture := perfFixtureFor(tb)

	perfRollupOnce.Do(func() {
		started := time.Now()

		if err := MaintainWindowRollups(fixture.seededAt); err != nil {
			tb.Fatalf("MaintainWindowRollups: %v", err)
		}

		perfRollupCost = time.Since(started)
	})

	return perfRollupCost
}

// TestPerfWindowRollupMaintenance reports the one-off bootstrap cost of the
// trailing window rollups and the cost of the daily expiry that follows. It is
// opt-in because it advances the fixture window, which invalidates the seeded
// 7-day rollup for any benchmark in the same process.
//
// Run it with DENYZER_PERF_MAINTENANCE=1 DENYZER_PERF_REUSE=1 go test ./storage/
// -run TestPerfWindowRollupMaintenance -v.
func TestPerfWindowRollupMaintenance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf maintenance in short mode")
	}

	if os.Getenv("DENYZER_PERF_MAINTENANCE") != "1" {
		t.Skip("set DENYZER_PERF_MAINTENANCE=1 to run")
	}

	fixture := perfFixtureFor(t)
	now := fixture.seededAt

	// Force the next maintenance to rebuild from the day buckets even if a
	// previous run already populated the rollups: a watermark ahead of the
	// window is treated as a lost watermark.
	forceWindowRebuild(t)

	started := time.Now()

	if err := MaintainWindowRollups(now); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	bootstrap := time.Since(started)

	started = time.Now()

	if err := MaintainWindowRollups(now); err != nil {
		t.Fatalf("steady state: %v", err)
	}

	steady := time.Since(started)

	started = time.Now()

	if err := MaintainWindowRollups(now.AddDate(0, 0, 1)); err != nil {
		t.Fatalf("expiry: %v", err)
	}

	expiry := time.Since(started)

	t.Logf("bootstrap=%s steady=%s expiry_one_day=%s", bootstrap, steady, expiry)

	for _, tc := range []struct {
		dimension   string
		granularity string
	}{
		{DimensionDomain, GranularityWindow7d},
		{DimensionDomain, GranularityWindow30d},
		{DimensionClient, GranularityWindow7d},
		{DimensionClient, GranularityWindow30d},
	} {
		started = time.Now()

		entities, err := GetTopEntities(tc.dimension, tc.granularity, 0, 0)
		if err != nil {
			t.Fatalf("GetTopEntities %s %s: %v", tc.dimension, tc.granularity, err)
		}

		t.Logf("%s/%s rows=%d read=%s", tc.dimension, tc.granularity, len(entities), time.Since(started))
	}
}

// forceWindowRebuild moves the rollup watermarks ahead of the window so the
// next maintenance run treats them as lost and rebuilds from the day buckets.
func forceWindowRebuild(tb testing.TB) {
	tb.Helper()

	future := WindowStart(time.Now(), window30dDays) + int64(365*24*time.Hour/time.Second)

	for _, spec := range WindowSpecs() {
		if err := writeWindowStart(spec.Granularity, future); err != nil {
			tb.Fatalf("reset %s watermark: %v", spec.Granularity, err)
		}
	}
}

// TestPerfSeed seeds the million-row fixture once and reports the row counts and
// the seeding time. Run it with -run TestPerfSeed -v to inspect the fixture
// without running the benchmarks.
func TestPerfSeed(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping perf seed in short mode")
	}

	if os.Getenv("DENYZER_PERF_SEED") != "1" {
		t.Skip("set DENYZER_PERF_SEED=1 to seed the million-row fixture")
	}

	fixture := perfFixtureFor(t)

	t.Logf(
		"queries=%d metric_rows=%d (domain/day=%d, domain/hour=%d, domain/minute=%d, "+
			"client/day=%d, client/hour=%d, client/minute=%d) db=%s seeded_in=%s",
		fixture.queryCount, fixture.totalMetricRows,
		fixture.domainDayRows, fixture.domainHourRows, fixture.domainMinRows,
		fixture.clientDayRows, fixture.clientHourRows, fixture.clientMinRows,
		fixture.path, fixture.seedDuration,
	)
}

// BenchmarkTopEntitiesDayBuckets measures the pre-rollup path: the 7d and 30d
// Top lists summing every day bucket of the metrics table. It is kept as the
// baseline the rollups are compared against.
func BenchmarkTopEntitiesDayBuckets(b *testing.B) {
	perfFixtureFor(b)

	cases := []struct {
		name      string
		dimension string
		days      int
	}{
		{"30d/domains", DimensionDomain, perfWindowDays},
		{"7d/domains", DimensionDomain, 7},
		{"1d/domains", DimensionDomain, 1},
		{"30d/clients", DimensionClient, perfWindowDays},
		{"7d/clients", DimensionClient, 7},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			granularity, start, end := perfWindow(tc.days)

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				entities, err := GetTopEntities(tc.dimension, granularity, start, end)
				if err != nil {
					b.Fatalf("GetTopEntities: %v", err)
				}

				if len(entities) == 0 {
					b.Fatal("GetTopEntities returned no entities")
				}
			}
		})
	}
}

// BenchmarkWindowRollups measures the dashboard path after the fix: the 7d and
// 30d Top lists read one rollup row per entity (50k domains, 500 clients)
// instead of one row per entity and day.
func BenchmarkWindowRollups(b *testing.B) {
	perfRollupsFor(b)

	cases := []struct {
		name        string
		dimension   string
		granularity string
	}{
		{"30d/domains", DimensionDomain, GranularityWindow30d},
		{"7d/domains", DimensionDomain, GranularityWindow7d},
		{"30d/clients", DimensionClient, GranularityWindow30d},
		{"7d/clients", DimensionClient, GranularityWindow7d},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				entities, err := GetTopEntities(tc.dimension, tc.granularity, 0, 0)
				if err != nil {
					b.Fatalf("GetTopEntities: %v", err)
				}

				if len(entities) == 0 {
					b.Fatal("GetTopEntities returned no entities")
				}
			}
		})
	}
}

// BenchmarkMetricScan separates the cost of the index range scan from the cost
// of materialising every row as a record. It walks the same series index range
// as BenchmarkTopEntities but never decodes the payload.
func BenchmarkMetricScan(b *testing.B) {
	perfFixtureFor(b)

	cases := []struct {
		name      string
		dimension string
		days      int
	}{
		{"30d/domains", DimensionDomain, perfWindowDays},
		{"7d/domains", DimensionDomain, 7},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			granularity, start, end := perfWindow(tc.days)

			conn, err := getDB()
			if err != nil {
				b.Fatal(err)
			}

			const scanSQL = `SELECT entity, total, blocked FROM metrics
WHERE granularity = ? AND dimension = ? AND bucket_start >= ? AND bucket_start <= ?`

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				if count := scanPerfMetrics(b, conn, scanSQL, granularity, tc.dimension, start, end); count == 0 {
					b.Fatal("scan returned no rows")
				}
			}
		})
	}
}

// scanPerfMetrics counts the rows of one index range scan without decoding the
// payload, so the benchmark measures scan cost only.
func scanPerfMetrics(tb testing.TB, conn *sql.DB, query string, args ...any) int {
	tb.Helper()

	rows, err := conn.QueryContext(context.Background(), query, args...)
	if err != nil {
		tb.Fatalf("scan: %v", err)
	}

	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			tb.Fatalf("scan: %v", closeErr)
		}
	}()

	count := 0

	for rows.Next() {
		var (
			entity  string
			total   int64
			blocked int64
		)

		if scanErr := rows.Scan(&entity, &total, &blocked); scanErr != nil {
			tb.Fatalf("scan: %v", scanErr)
		}

		count++
	}

	if err := rows.Err(); err != nil {
		tb.Fatalf("scan: %v", err)
	}

	return count
}

// BenchmarkMetricSeries measures the full GetMetricSeries path, which the
// dashboard chart uses: scan + record materialisation + sort.
func BenchmarkMetricSeries(b *testing.B) {
	perfFixtureFor(b)

	for _, tc := range []struct {
		name      string
		dimension string
		days      int
	}{
		{"30d/domains", DimensionDomain, perfWindowDays},
		{"7d/domains", DimensionDomain, 7},
	} {
		b.Run(tc.name, func(b *testing.B) {
			granularity, start, end := perfWindow(tc.days)

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				rows, err := GetMetricSeries(tc.dimension, granularity, "", start, end)
				if err != nil {
					b.Fatalf("GetMetricSeries: %v", err)
				}

				if len(rows) == 0 {
					b.Fatal("GetMetricSeries returned no rows")
				}
			}
		})
	}
}

// perfWindow mirrors topWindowBounds for a window of the given number of days.
func perfWindow(days int) (granularity string, start, end int64) {
	now := time.Now()
	startTime := now.AddDate(0, 0, -(days - 1))

	return GranularityDay, DayBucket(startTime), DayBucket(now)
}

// seedPerfFixture creates a fresh database, fills it with the simulated metric
// rows and returns the fixture metadata.
func seedPerfFixture(tb testing.TB) perfFixture {
	tb.Helper()

	queries := perfQueryCount(tb)

	// The fixture is shared by every benchmark in the process, so it cannot
	// live in a tb.TempDir() that is removed when the first test finishes.
	// Reuse a stable directory and start from scratch on each run.
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("denyzer-perf-%d", queries))
	dbPath := filepath.Join(dir, "perf.db")

	// DENYZER_PERF_REUSE=1 opens an existing fixture instead of rebuilding it,
	// so profiles and repeated benchmark runs skip the (slow) seed.
	if os.Getenv("DENYZER_PERF_REUSE") == "1" {
		if _, err := os.Stat(dbPath); err == nil {
			resetStorageState()

			if err := InitDB(dbPath); err != nil {
				tb.Fatalf("InitDB: %v", err)
			}

			return perfFixture{
				path:       dbPath,
				queryCount: queries,
				seededAt:   time.Now().Truncate(time.Hour),
			}
		}
	}

	if err := os.RemoveAll(dir); err != nil {
		tb.Fatalf("remove old perf dir: %v", err)
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		tb.Fatalf("create perf dir: %v", err)
	}

	fixture := perfFixture{
		path:       dbPath,
		queryCount: queries,
		seededAt:   time.Now().Truncate(time.Hour),
	}

	resetStorageState()

	if err := InitDB(fixture.path); err != nil {
		tb.Fatalf("InitDB: %v", err)
	}

	started := time.Now()
	batch := make([]MetricRow, 0, perfSeedBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}

		if err := insertPerfRows(batch); err != nil {
			tb.Fatalf("seed: %v", err)
		}

		batch = batch[:0]
	}

	add := func(row MetricRow) {
		batch = append(batch, row)

		if len(batch) >= perfSeedBatchSize {
			flush()
		}
	}

	// One metric row per query for the domain/day dimension: every simulated
	// query lands on a distinct (domain, day) pair.
	dayBuckets := make([]int64, perfWindowDays)
	for i := range dayBuckets {
		day := fixture.seededAt.AddDate(0, 0, -(perfWindowDays - 1 - i))
		dayBuckets[i] = DayBucket(day)
	}

	hourBuckets := make([]int64, perfWindowDays*24)
	for i := range hourBuckets {
		hourBuckets[i] = fixture.seededAt.Add(-time.Duration(len(hourBuckets)-1-i) * time.Hour).Unix()
	}

	minuteBuckets := make([]int64, perfWindowDays*24*60)
	for i := range minuteBuckets {
		minuteBuckets[i] = fixture.seededAt.Add(-time.Duration(len(minuteBuckets)-1-i) * time.Minute).Unix()
	}

	for i := range queries {
		day := dayBuckets[i%len(dayBuckets)]
		domain := fmt.Sprintf("d%06d.example", (i*perfUniqueDomains/queries)%perfUniqueDomains)
		blocked := uint64(0)

		if i%100 < perfBlockedPercent {
			blocked = 1
		}

		total := uint64(1)
		forwarded := total - blocked

		add(MetricRow{
			ID: metricID(DimensionDomain, GranularityDay, day, domain),
			// Real rows carry both the entity key and its label.
			Dimension:   DimensionDomain,
			Granularity: GranularityDay,
			BucketStart: day,
			Entity:      domain,
			EntityLabel: domain,
			Total:       total,
			Blocked:     blocked,
			Forwarded:   forwarded,
		})
		fixture.domainDayRows++

		// Hourly and per-minute domain rows: one per query as well, because
		// each query falls into a distinct bucket at these granularities.
		hour := hourBuckets[i%len(hourBuckets)]
		minute := minuteBuckets[i%len(minuteBuckets)]

		add(MetricRow{
			ID:          metricID(DimensionDomain, GranularityHour, hour, domain),
			Dimension:   DimensionDomain,
			Granularity: GranularityHour,
			BucketStart: hour,
			Entity:      domain,
			EntityLabel: domain,
			Total:       total,
			Blocked:     blocked,
			Forwarded:   forwarded,
		})
		fixture.domainHourRows++

		add(MetricRow{
			ID:          metricID(DimensionDomain, GranularityMinute, minute, domain),
			Dimension:   DimensionDomain,
			Granularity: GranularityMinute,
			BucketStart: minute,
			Entity:      domain,
			EntityLabel: domain,
			Total:       total,
			Blocked:     blocked,
			Forwarded:   forwarded,
		})
		fixture.domainMinRows++
	}

	// Clients repeat over the whole window, so their rows collapse onto the
	// cartesian product of client and bucket: 500 * 30 days, 500 * 720 hours
	// and 500 * 1440 minutes, capped by the number of queries.
	seedPerfClients(add, &fixture, queries, dayBuckets, hourBuckets, minuteBuckets)

	flush()

	// Global buckets: one row per bucket, matching the writer.
	for _, bucket := range dayBuckets {
		add(MetricRow{
			ID:          metricID(DimensionGlobal, GranularityDay, bucket, ""),
			Dimension:   DimensionGlobal,
			Granularity: GranularityDay,
			BucketStart: bucket,
			Total:       uint64(queries),
		})
	}

	for _, bucket := range hourBuckets {
		add(MetricRow{
			ID:          metricID(DimensionGlobal, GranularityHour, bucket, ""),
			Dimension:   DimensionGlobal,
			Granularity: GranularityHour,
			BucketStart: bucket,
			Total:       uint64(queries),
		})
	}

	for _, bucket := range minuteBuckets {
		add(MetricRow{
			ID:          metricID(DimensionGlobal, GranularityMinute, bucket, ""),
			Dimension:   DimensionGlobal,
			Granularity: GranularityMinute,
			BucketStart: bucket,
			Total:       uint64(queries),
		})
	}

	add(MetricRow{
		ID:        metricID(DimensionGlobal, GranularityAllTime, 0, ""),
		Dimension: DimensionGlobal,
		// An all-time row has no bucket.
		Granularity: GranularityAllTime,
		Total:       uint64(queries),
	})

	flush()

	fixture.seedDuration = time.Since(started)
	fixture.totalMetricRows = countMetricRows(tb)

	return fixture
}

// seedPerfClients adds the client metric rows. Each client appears in every
// day bucket, in the hour buckets until the query budget is spent, and in the
// last queries/perfUniqueClients minute buckets.
func seedPerfClients(
	add func(MetricRow), fixture *perfFixture, queries int,
	dayBuckets, hourBuckets, minuteBuckets []int64,
) {
	perClientMinutes := queries / perfUniqueClients
	minuteStart := max(0, len(minuteBuckets)-perClientMinutes)

	for d := range perfUniqueClients {
		client := fmt.Sprintf("client-%03d", d)
		blocked := uint64(0)

		if d%10 == 0 {
			blocked = 1
		}

		total := uint64(1)
		forwarded := total - blocked

		for _, bucket := range dayBuckets {
			add(clientMetricRow(GranularityDay, bucket, client, total, blocked, forwarded))
			fixture.clientDayRows++
		}

		if d*len(hourBuckets) >= queries {
			continue
		}

		for _, bucket := range hourBuckets {
			add(clientMetricRow(GranularityHour, bucket, client, total, blocked, forwarded))
			fixture.clientHourRows++
		}

		for _, bucket := range minuteBuckets[minuteStart:] {
			add(clientMetricRow(GranularityMinute, bucket, client, total, blocked, forwarded))
			fixture.clientMinRows++
		}
	}
}

func clientMetricRow(
	granularity string, bucket int64, client string, total, blocked, forwarded uint64,
) MetricRow {
	return MetricRow{
		ID:          metricID(DimensionClient, granularity, bucket, client),
		Dimension:   DimensionClient,
		Granularity: granularity,
		BucketStart: bucket,
		Entity:      client,
		EntityLabel: client,
		Total:       total,
		Blocked:     blocked,
		Forwarded:   forwarded,
	}
}

// insertPerfRows writes rows straight into the metrics table with absolute
// values; the fixture keys are unique by construction.
func insertPerfRows(rows []MetricRow) error {
	batch, err := NewBatch()
	if err != nil {
		return err
	}

	for i := range rows {
		if err := putMetricRow(batch.tx, &rows[i]); err != nil {
			RollbackBatch(batch)

			return fmt.Errorf("insert metric %s: %w", rows[i].ID, err)
		}
	}

	return CommitBatch(batch)
}

// countMetricRows counts every row of the metrics table.
func countMetricRows(tb testing.TB) int {
	tb.Helper()

	conn, err := getDB()
	if err != nil {
		tb.Fatalf("count metrics: %v", err)
	}

	var count int

	if err := conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM metrics").Scan(&count); err != nil {
		tb.Fatalf("count metrics: %v", err)
	}

	return count
}
