package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// storageFixture holds the seeded state shared by the storage subtests.
//
// InitDB is a process-wide singleton, so every assertion shares one database
// path and runs in order inside the single test function.
type storageFixture struct {
	dbPath       string
	minuteBucket int64
	hourBucket   int64
	dayBucket    int64
	entryCount   int
}

// TestStorage exercises the two-table store end to end: atomic query-log and
// metric writes, exact metric upserts, inclusive series reads, cursor
// pagination with client filters and the retention cleanup rules.
func TestStorage(t *testing.T) {
	fixture := setupStorageFixture(t)

	t.Run("QueryLogsReadBack", func(t *testing.T) { testQueryLogsReadBack(t, fixture) })
	t.Run("QueryLogsCursorPagination", func(t *testing.T) { testQueryLogsCursorPagination(t, fixture) })
	t.Run("QueryLogsClientFilterMatchesNameOrIP", func(t *testing.T) { testQueryLogsClientFilter(t, fixture) })
	t.Run("MetricSeriesIncludesEndBucket", func(t *testing.T) { testMetricSeriesIncludesEndBucket(t, fixture) })
	t.Run("MetricSeriesRange", func(t *testing.T) { testMetricSeriesRange(t, fixture) })
	t.Run("EntitySeries", func(t *testing.T) { testEntitySeries(t, fixture) })
	t.Run("TopEntitiesKeepsEverySourceBucket", func(t *testing.T) { testTopEntities(t, fixture) })
	t.Run("QueryTypeCounts", func(t *testing.T) { testQueryTypeCounts(t, fixture) })
	t.Run("AllTimeCounter", func(t *testing.T) { testAllTimeCounter(t, fixture) })
	t.Run("DuplicateMetricIDsAreMerged", func(t *testing.T) { testDuplicateMetricIDsAreMerged(t, fixture) })
	t.Run("CleanupKeepsAllTimeAndGlobalDaily", func(t *testing.T) { testCleanupKeepsAllTime(t, fixture) })
	t.Run("CleanupKeepsRollupWindowDays", func(t *testing.T) { testCleanupKeepsRollupDays(t, fixture) })
	t.Run("AutoIncrementSurvivesRestart", func(t *testing.T) { testAutoIncrementSurvivesRestart(t, fixture) })
	t.Run("WindowRollupsMatchDayBuckets", func(t *testing.T) { testWindowRollups(t, fixture) })
	t.Run("WindowRollupRebuildDropsStaleRows", func(t *testing.T) { testWindowRollupRebuild(t, fixture) })
	t.Run("WindowRollupsExpireDays", func(t *testing.T) { testWindowRollupExpiry(t, fixture) })
}

// setupStorageFixture opens the shared database and seeds it with one
// atomic batch of query logs and metrics.
func setupStorageFixture(t *testing.T) storageFixture {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")

	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	now := time.Now().Truncate(time.Hour).Add(5 * time.Minute)
	minuteBucket := BucketStart(now, GranularityMinute)
	hourBucket := BucketStart(now, GranularityHour)
	dayBucket := BucketStart(now, GranularityDay)

	entries := []*QueryLogEntry{
		{
			Timestamp: now.UnixMilli(), ClientIP: "10.0.0.1", ClientName: "laptop",
			Domain: "example.com", QueryType: "A", ResponseType: "RESOLVED", Reason: "RESOLVED", DurationMs: 12,
		},
		{
			Timestamp: now.UnixMilli(), ClientIP: "10.0.0.1", ClientName: "laptop",
			Domain: "blocked.com", QueryType: "A", ResponseType: "BLOCKED", Reason: "BLOCKED", DurationMs: 3,
		},
		{
			Timestamp: now.UnixMilli(), ClientIP: "10.0.0.2", ClientName: "",
			Domain: "example.org", QueryType: "AAAA", ResponseType: "RESOLVED", Reason: "RESOLVED", DurationMs: 9,
		},
		{
			Timestamp: now.UnixMilli(), ClientIP: "10.0.0.2", ClientName: "",
			Domain: "cached.com", QueryType: "A", ResponseType: "CACHED", Reason: "CACHED", DurationMs: 1,
		},
		{
			Timestamp: now.Add(-time.Millisecond).UnixMilli(), ClientIP: "10.0.0.3", ClientName: "phone",
			Domain: "old.com", QueryType: "A", ResponseType: "BLOCKED", Reason: "BLOCKED", DurationMs: 5,
		},
	}

	metrics := []MetricInput{
		{
			Dimension: DimensionGlobal, Granularity: GranularityMinute, BucketStart: minuteBucket,
			Delta: MetricsDelta{Total: 2, Blocked: 1, Cached: 1, Forwarded: 1, TotalLatencyMs: 25},
		},
		{
			Dimension: DimensionGlobal, Granularity: GranularityHour, BucketStart: hourBucket,
			Delta: MetricsDelta{Total: 5, Blocked: 2, Cached: 1, Forwarded: 2, TotalLatencyMs: 30},
		},
		{
			Dimension: DimensionGlobal, Granularity: GranularityDay, BucketStart: dayBucket,
			Delta: MetricsDelta{Total: 5, Blocked: 2, Cached: 1, Forwarded: 2, TotalLatencyMs: 30},
		},
		{
			Dimension: DimensionGlobal, Granularity: GranularityAllTime, BucketStart: 0,
			Delta: MetricsDelta{Total: 5, Blocked: 2, Cached: 1, Forwarded: 2, TotalLatencyMs: 30},
		},
		{
			Dimension: DimensionDomain, Granularity: GranularityHour, BucketStart: hourBucket,
			Entity: "example.com", EntityLabel: "example.com",
			Delta: MetricsDelta{Total: 1, Forwarded: 1, TotalLatencyMs: 12},
		},
		{
			Dimension: DimensionDomain, Granularity: GranularityHour, BucketStart: hourBucket,
			Entity: "blocked.com", EntityLabel: "blocked.com",
			Delta: MetricsDelta{Total: 1, Blocked: 1, TotalLatencyMs: 3},
		},
		{
			Dimension: DimensionClient, Granularity: GranularityHour, BucketStart: hourBucket,
			Entity: "laptop", EntityLabel: "laptop",
			Delta: MetricsDelta{Total: 2, Blocked: 1, Forwarded: 1, TotalLatencyMs: 15},
		},
		{
			Dimension: DimensionClient, Granularity: GranularityHour, BucketStart: hourBucket,
			Entity: "10.0.0.2", EntityLabel: "10.0.0.2",
			Delta: MetricsDelta{Total: 2, Cached: 1, Forwarded: 1, TotalLatencyMs: 10},
		},
		{
			Dimension: DimensionQueryType, Granularity: GranularityDay, BucketStart: dayBucket,
			Entity: "A", EntityLabel: "A", Delta: MetricsDelta{Total: 3, Blocked: 1},
		},
		{
			Dimension: DimensionQueryType, Granularity: GranularityAllTime, BucketStart: 0,
			Entity: "A", EntityLabel: "A", Delta: MetricsDelta{Total: 3, Blocked: 1},
		},
	}

	batch, err := NewBatch()
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}

	if err := BatchInsertQueryLogs(entries, batch); err != nil {
		t.Fatalf("BatchInsertQueryLogs: %v", err)
	}

	if err := UpsertMetrics(batch, metrics); err != nil {
		t.Fatalf("UpsertMetrics: %v", err)
	}

	if err := CommitBatch(batch); err != nil {
		t.Fatalf("CommitBatch: %v", err)
	}

	return storageFixture{
		dbPath:       dbPath,
		minuteBucket: minuteBucket,
		hourBucket:   hourBucket,
		dayBucket:    dayBucket,
		entryCount:   len(entries),
	}
}

// testQueryLogsReadBack verifies that the seeded query logs are returned
// newest first and carry a primary key.
func testQueryLogsReadBack(t *testing.T, fixture storageFixture) {
	t.Helper()

	page, err := GetQueryLogsPage("", 10, "", "", "", "")
	if err != nil {
		t.Fatalf("GetQueryLogsPage: %v", err)
	}

	if len(page.Entries) != fixture.entryCount {
		t.Fatalf("got %d entries, want %d", len(page.Entries), fixture.entryCount)
	}

	if page.HasMore {
		t.Fatal("unexpected HasMore for a full page")
	}

	// Newest first: the last inserted entry has the highest id.
	first := page.Entries[0]
	if first.ClientIP != "10.0.0.3" {
		t.Fatalf("first entry client = %q, want 10.0.0.3", first.ClientIP)
	}

	if first.ID == 0 {
		t.Fatal("query log entry has no primary key")
	}
}

// testQueryLogsCursorPagination walks the cursor pages and verifies that
// every entry is returned exactly once.
func testQueryLogsCursorPagination(t *testing.T, fixture storageFixture) {
	t.Helper()

	seen := make(map[int]bool)
	cursor := ""

	for range 10 {
		page, err := GetQueryLogsPage(cursor, 2, "", "", "", "")
		if err != nil {
			t.Fatalf("GetQueryLogsPage: %v", err)
		}

		for _, entry := range page.Entries {
			if seen[entry.ID] {
				t.Fatalf("entry %d returned twice", entry.ID)
			}

			seen[entry.ID] = true
		}

		if !page.HasMore {
			break
		}

		cursor = page.NextCursor
	}

	if len(seen) != fixture.entryCount {
		t.Fatalf("pagination returned %d unique entries, want %d", len(seen), fixture.entryCount)
	}
}

// testQueryLogsClientFilter verifies that the client filter matches either
// the client name or the client IP.
func testQueryLogsClientFilter(t *testing.T, _ storageFixture) {
	t.Helper()

	byName, err := GetQueryLogsPage("", 10, "", "", "laptop", "")
	if err != nil {
		t.Fatalf("GetQueryLogsPage: %v", err)
	}

	if len(byName.Entries) != 2 {
		t.Fatalf("client name filter returned %d entries, want 2", len(byName.Entries))
	}

	byIP, err := GetQueryLogsPage("", 10, "", "", "10.0.0.2", "")
	if err != nil {
		t.Fatalf("GetQueryLogsPage: %v", err)
	}

	if len(byIP.Entries) != 2 {
		t.Fatalf("client IP filter returned %d entries, want 2", len(byIP.Entries))
	}
}

// testMetricSeriesIncludesEndBucket verifies that the range end is inclusive.
func testMetricSeriesIncludesEndBucket(t *testing.T, fixture storageFixture) {
	t.Helper()

	rows, err := GetMetricSeries(DimensionGlobal, GranularityHour, "", fixture.hourBucket, fixture.hourBucket)
	if err != nil {
		t.Fatalf("GetMetricSeries: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d rows for a single bucket, want 1", len(rows))
	}

	if rows[0].Total != 5 || rows[0].Blocked != 2 {
		t.Fatalf("bucket totals = %d/%d, want 5/2", rows[0].Total, rows[0].Blocked)
	}
}

// testMetricSeriesRange verifies that buckets outside the range are skipped.
func testMetricSeriesRange(t *testing.T, fixture storageFixture) {
	t.Helper()

	rows, err := GetMetricSeries(DimensionGlobal, GranularityHour, "", fixture.hourBucket-7200, fixture.hourBucket)
	if err != nil {
		t.Fatalf("GetMetricSeries: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d rows in range, want 1", len(rows))
	}

	empty, err := GetMetricSeries(
		DimensionGlobal, GranularityHour, "", fixture.hourBucket+3600, fixture.hourBucket+7200,
	)
	if err != nil {
		t.Fatalf("GetMetricSeries: %v", err)
	}

	if len(empty) != 0 {
		t.Fatalf("got %d rows outside the range, want 0", len(empty))
	}
}

// testEntitySeries verifies that entity scoped series are read with their
// label attached.
func testEntitySeries(t *testing.T, fixture storageFixture) {
	t.Helper()

	rows, err := GetMetricSeries(DimensionClient, GranularityHour, "laptop", fixture.hourBucket, fixture.hourBucket)
	if err != nil {
		t.Fatalf("GetMetricSeries: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d client rows, want 1", len(rows))
	}

	if rows[0].Total != 2 || rows[0].Blocked != 1 {
		t.Fatalf("client totals = %d/%d, want 2/1", rows[0].Total, rows[0].Blocked)
	}

	if rows[0].EntityLabel != "laptop" {
		t.Fatalf("client label = %q, want laptop", rows[0].EntityLabel)
	}
}

// testTopEntities verifies that the ranked list keeps every source bucket
// instead of pruning them by rank.
func testTopEntities(t *testing.T, fixture storageFixture) {
	t.Helper()

	entities, err := GetTopEntities(DimensionDomain, GranularityHour, fixture.hourBucket-3600, fixture.hourBucket)
	if err != nil {
		t.Fatalf("GetTopEntities: %v", err)
	}

	if len(entities) != 2 {
		t.Fatalf("got %d entities, want 2", len(entities))
	}

	byName := make(map[string]EntityMetric, len(entities))
	for _, entity := range entities {
		byName[entity.Entity] = entity
	}

	if got := byName["blocked.com"].Blocked; got != 1 {
		t.Fatalf("blocked.com blocked = %d, want 1", got)
	}

	if got := byName["example.com"].Total; got != 1 {
		t.Fatalf("example.com total = %d, want 1", got)
	}
}

// testQueryTypeCounts verifies the query type breakdown.
func testQueryTypeCounts(t *testing.T, fixture storageFixture) {
	t.Helper()

	counts, err := GetQueryTypeCounts(GranularityDay, fixture.dayBucket, fixture.dayBucket)
	if err != nil {
		t.Fatalf("GetQueryTypeCounts: %v", err)
	}

	if counts["A"] != 3 {
		t.Fatalf("query type A count = %d, want 3", counts["A"])
	}
}

// testAllTimeCounter verifies that the all-time counter reflects the seeded
// query logs.
func testAllTimeCounter(t *testing.T, _ storageFixture) {
	t.Helper()

	total, err := GetQueryLogsTotal()
	if err != nil {
		t.Fatalf("GetQueryLogsTotal: %v", err)
	}

	if total != 5 {
		t.Fatalf("all-time total = %d, want 5", total)
	}
}

// testDuplicateMetricIDsAreMerged verifies that two inputs targeting the same
// metric are merged into a single row.
func testDuplicateMetricIDsAreMerged(t *testing.T, fixture storageFixture) {
	t.Helper()

	batch, err := NewBatch()
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}

	duplicates := []MetricInput{
		{
			Dimension: DimensionGlobal, Granularity: GranularityHour, BucketStart: fixture.hourBucket,
			Delta: MetricsDelta{Total: 2, Forwarded: 2},
		},
		{
			Dimension: DimensionGlobal, Granularity: GranularityHour, BucketStart: fixture.hourBucket,
			Delta: MetricsDelta{Total: 3, Cached: 3},
		},
	}

	if err := UpsertMetrics(batch, duplicates); err != nil {
		t.Fatalf("UpsertMetrics: %v", err)
	}

	if err := CommitBatch(batch); err != nil {
		t.Fatalf("CommitBatch: %v", err)
	}

	rows, err := GetMetricSeries(DimensionGlobal, GranularityHour, "", fixture.hourBucket, fixture.hourBucket)
	if err != nil {
		t.Fatalf("GetMetricSeries: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("duplicate metric IDs produced %d rows, want 1", len(rows))
	}

	if rows[0].Total != 10 || rows[0].Cached != 4 || rows[0].Forwarded != 4 {
		t.Fatalf("merged totals = %d/%d/%d, want 10/4/4",
			rows[0].Total, rows[0].Cached, rows[0].Forwarded)
	}
}

// testCleanupKeepsAllTime verifies that the cleanup removes expired history
// while preserving the all-time counter and the global daily metrics.
func testCleanupKeepsAllTime(t *testing.T, _ storageFixture) {
	t.Helper()

	old := time.Now().AddDate(0, -3, 0)
	oldHour := BucketStart(old, GranularityHour)
	oldDay := BucketStart(old, GranularityDay)

	batch, err := NewBatch()
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}

	oldMetrics := []MetricInput{
		{
			Dimension: DimensionGlobal, Granularity: GranularityHour, BucketStart: oldHour,
			Delta: MetricsDelta{Total: 3},
		},
		{
			Dimension: DimensionGlobal, Granularity: GranularityDay, BucketStart: oldDay,
			Delta: MetricsDelta{Total: 3},
		},
		{
			Dimension: DimensionDomain, Granularity: GranularityHour, BucketStart: oldHour,
			Entity: "old.com", EntityLabel: "old.com", Delta: MetricsDelta{Total: 3},
		},
	}

	if err := UpsertMetrics(batch, oldMetrics); err != nil {
		t.Fatalf("UpsertMetrics: %v", err)
	}

	if err := CommitBatch(batch); err != nil {
		t.Fatalf("CommitBatch: %v", err)
	}

	result, err := CleanUp(1)
	if err != nil {
		t.Fatalf("CleanUp: %v", err)
	}

	if result.MetricsDeleted == 0 {
		t.Fatal("cleanup deleted no metrics")
	}

	assertMetricSeriesEmpty(t, DimensionGlobal, GranularityHour, "", oldHour, oldHour,
		"old global hour bucket survived cleanup")

	dayRows, err := GetMetricSeries(DimensionGlobal, GranularityDay, "", oldDay, oldDay)
	if err != nil {
		t.Fatalf("GetMetricSeries: %v", err)
	}

	if len(dayRows) != 1 {
		t.Fatal("global daily metrics must survive cleanup")
	}

	assertMetricSeriesEmpty(t, DimensionDomain, GranularityHour, "old.com", oldHour, oldHour,
		"old domain metric survived cleanup")

	total, err := GetQueryLogsTotal()
	if err != nil {
		t.Fatalf("GetQueryLogsTotal: %v", err)
	}

	if total != 5 {
		t.Fatalf("all-time total after cleanup = %d, want 5", total)
	}
}

// testCleanupKeepsRollupDays verifies that the retention cleanup keeps the day
// buckets the trailing 30-day rollup still needs to expire.
func testCleanupKeepsRollupDays(t *testing.T, _ storageFixture) {
	t.Helper()

	now := time.Now()
	// The day that leaves the 30-day window today, plus one day of margin.
	lastNeeded := DayBucket(now.AddDate(0, 0, -(window30dDays + rollupRetentionMarginDays)))
	tooOld := DayBucket(now.AddDate(0, 0, -(window30dDays + rollupRetentionMarginDays + 5)))

	batch, err := NewBatch()
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}

	err = UpsertMetrics(batch, []MetricInput{
		{
			Dimension: DimensionDomain, Granularity: GranularityDay, BucketStart: lastNeeded,
			Entity: "rollup-margin.test", EntityLabel: "rollup-margin.test",
			Delta: MetricsDelta{Total: 2},
		},
		{
			Dimension: DimensionDomain, Granularity: GranularityDay, BucketStart: tooOld,
			Entity: "rollup-too-old.test", EntityLabel: "rollup-too-old.test",
			Delta: MetricsDelta{Total: 2},
		},
	})
	if err != nil {
		t.Fatalf("UpsertMetrics: %v", err)
	}

	if err := CommitBatch(batch); err != nil {
		t.Fatalf("CommitBatch: %v", err)
	}

	if _, err := CleanUp(1); err != nil {
		t.Fatalf("CleanUp: %v", err)
	}

	kept, err := GetMetricSeries(DimensionDomain, GranularityDay, "rollup-margin.test",
		lastNeeded, lastNeeded)
	if err != nil {
		t.Fatalf("GetMetricSeries: %v", err)
	}

	if len(kept) != 1 {
		t.Fatal("cleanup removed the day bucket the rollup still needs")
	}

	assertMetricSeriesEmpty(t, DimensionDomain, GranularityDay, "rollup-too-old.test",
		tooOld, tooOld, "cleanup kept a day bucket that is beyond the rollup margin")
}

// assertMetricSeriesEmpty fails the test when the requested series contains
// any row.
func assertMetricSeriesEmpty(
	t *testing.T, dimension, granularity, entity string, start, end int64, message string,
) {
	t.Helper()

	rows, err := GetMetricSeries(dimension, granularity, entity, start, end)
	if err != nil {
		t.Fatalf("GetMetricSeries: %v", err)
	}

	if len(rows) != 0 {
		t.Fatal(message)
	}
}

// testAutoIncrementSurvivesRestart verifies that reopening the database keeps
// the stored rows reachable and that new rows get fresh identifiers.
func testAutoIncrementSurvivesRestart(t *testing.T, fixture storageFixture) {
	t.Helper()

	if err := CloseDB(); err != nil {
		t.Fatalf("CloseDB: %v", err)
	}

	resetStorageState()

	if err := InitDB(fixture.dbPath); err != nil {
		t.Fatalf("InitDB after restart: %v", err)
	}

	batch, err := NewBatch()
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}

	restartMetrics := []MetricInput{
		{
			Dimension: DimensionGlobal, Granularity: GranularityMinute, BucketStart: fixture.minuteBucket + 60,
			Delta: MetricsDelta{Total: 1},
		},
	}

	if err := UpsertMetrics(batch, restartMetrics); err != nil {
		t.Fatalf("UpsertMetrics: %v", err)
	}

	if err := CommitBatch(batch); err != nil {
		t.Fatalf("CommitBatch: %v", err)
	}

	// The row written before the restart must still be there.
	rows, err := GetMetricSeries(
		DimensionGlobal, GranularityMinute, "", fixture.minuteBucket, fixture.minuteBucket,
	)
	if err != nil {
		t.Fatalf("GetMetricSeries: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("got %d rows for the pre-restart bucket, want 1", len(rows))
	}

	if rows[0].Total != 2 {
		t.Fatalf("pre-restart bucket total = %d, want 2", rows[0].Total)
	}

	// The new row must not have overwritten an existing one.
	newRows, err := GetMetricSeries(
		DimensionGlobal, GranularityMinute, "", fixture.minuteBucket+60, fixture.minuteBucket+60,
	)
	if err != nil {
		t.Fatalf("GetMetricSeries: %v", err)
	}

	if len(newRows) != 1 {
		t.Fatalf("got %d rows for the post-restart bucket, want 1", len(newRows))
	}

	if newRows[0].Total != 1 {
		t.Fatalf("post-restart bucket total = %d, want 1", newRows[0].Total)
	}
}

// testWindowRollups verifies that the trailing window rollup is rebuilt from
// the day buckets and ranks the same entities.
func testWindowRollups(t *testing.T, _ storageFixture) {
	t.Helper()

	now := time.Now()
	today := DayBucket(now)
	yesterday := DayBucket(now.AddDate(0, 0, -1))

	inputs := []MetricInput{
		{
			Dimension: DimensionDomain, Granularity: GranularityDay, BucketStart: today,
			Entity: "rollup-a.test", EntityLabel: "rollup-a.test",
			Delta: MetricsDelta{Total: 5, Blocked: 1, Forwarded: 4},
		},
		{
			Dimension: DimensionDomain, Granularity: GranularityDay, BucketStart: yesterday,
			Entity: "rollup-a.test", EntityLabel: "rollup-a.test",
			Delta: MetricsDelta{Total: 2, Blocked: 1, Forwarded: 1},
		},
		{
			Dimension: DimensionDomain, Granularity: GranularityDay, BucketStart: today,
			Entity: "rollup-b.test", EntityLabel: "rollup-b.test",
			Delta: MetricsDelta{Total: 3, Blocked: 3},
		},
		{
			Dimension: DimensionClient, Granularity: GranularityDay, BucketStart: today,
			Entity: "rollup-client", EntityLabel: "rollup-client",
			Delta: MetricsDelta{Total: 8, Blocked: 2},
		},
	}

	batch, err := NewBatch()
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}

	if err := UpsertMetrics(batch, inputs); err != nil {
		t.Fatalf("UpsertMetrics: %v", err)
	}

	if err := CommitBatch(batch); err != nil {
		t.Fatalf("CommitBatch: %v", err)
	}

	if err := MaintainWindowRollups(now); err != nil {
		t.Fatalf("MaintainWindowRollups: %v", err)
	}

	if !WindowRollupsReady() {
		t.Fatal("window rollups are not ready after maintenance")
	}

	domains, err := GetTopEntities(DimensionDomain, GranularityWindow7d, 0, 0)
	if err != nil {
		t.Fatalf("GetTopEntities: %v", err)
	}

	byEntity := make(map[string]EntityMetric, len(domains))

	for _, entity := range domains {
		byEntity[entity.Entity] = entity
	}

	alpha, ok := byEntity["rollup-a.test"]
	if !ok {
		t.Fatal("rollup-a.test missing from the 7d rollup")
	}

	if alpha.Total != 7 || alpha.Blocked != 2 {
		t.Fatalf("rollup-a.test = total %d blocked %d, want 7/2", alpha.Total, alpha.Blocked)
	}

	clients, err := GetTopEntities(DimensionClient, GranularityWindow30d, 0, 0)
	if err != nil {
		t.Fatalf("GetTopEntities client: %v", err)
	}

	if len(clients) != 1 || clients[0].Total != 8 || clients[0].Blocked != 2 {
		t.Fatalf("client rollup = %+v, want one entity with 8/2", clients)
	}
}

// testWindowRollupRebuild verifies that a forced rebuild recomputes the
// rollup from the day buckets and drops rows that no longer have a source.
func testWindowRollupRebuild(t *testing.T, _ storageFixture) {
	t.Helper()

	batch, err := NewBatch()
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}

	err = UpsertMetrics(batch, []MetricInput{{
		Dimension: DimensionDomain, Granularity: GranularityWindow7d, BucketStart: 0,
		Entity: "stale.test", EntityLabel: "stale.test", Delta: MetricsDelta{Total: 99},
	}})
	if err != nil {
		t.Fatalf("UpsertMetrics: %v", err)
	}

	if err := CommitBatch(batch); err != nil {
		t.Fatalf("CommitBatch: %v", err)
	}

	// A watermark ahead of the window (clock moved back or a corrupt row)
	// forces a full rebuild from the day buckets.
	future := DayBucket(time.Now().AddDate(0, 0, 1))

	for _, spec := range WindowSpecs() {
		if err := writeWindowStart(spec.Granularity, future); err != nil {
			t.Fatalf("writeWindowStart: %v", err)
		}
	}

	if err := MaintainWindowRollups(time.Now()); err != nil {
		t.Fatalf("MaintainWindowRollups: %v", err)
	}

	domains, err := GetTopEntities(DimensionDomain, GranularityWindow7d, 0, 0)
	if err != nil {
		t.Fatalf("GetTopEntities: %v", err)
	}

	for _, entity := range domains {
		if entity.Entity == "stale.test" {
			t.Fatalf("stale rollup row survived the rebuild: %+v", entity)
		}
	}

	// The source entities are still present after the rebuild.
	byEntity := make(map[string]EntityMetric, len(domains))
	for _, entity := range domains {
		byEntity[entity.Entity] = entity
	}

	if alpha := byEntity["rollup-a.test"]; alpha.Total != 7 || alpha.Blocked != 2 {
		t.Fatalf("rollup-a.test after rebuild = %+v, want total 7 blocked 2", alpha)
	}
}

// testWindowRollupExpiry verifies that the day that leaves a window is
// subtracted from its rollup while the longer window keeps it.
func testWindowRollupExpiry(t *testing.T, _ storageFixture) {
	t.Helper()

	// Eight days later the seeded days are outside the 7-day window but still
	// inside the 30-day window.
	later := time.Now().AddDate(0, 0, 8)

	if err := MaintainWindowRollups(later); err != nil {
		t.Fatalf("MaintainWindowRollups: %v", err)
	}

	short, err := GetTopEntities(DimensionDomain, GranularityWindow7d, 0, 0)
	if err != nil {
		t.Fatalf("GetTopEntities 7d: %v", err)
	}

	if len(short) != 0 {
		t.Fatalf("7d rollup still has %d entities after expiry: %+v", len(short), short)
	}

	long, err := GetTopEntities(DimensionDomain, GranularityWindow30d, 0, 0)
	if err != nil {
		t.Fatalf("GetTopEntities 30d: %v", err)
	}

	found := false

	for _, entity := range long {
		if entity.Entity == "rollup-a.test" && entity.Total == 7 {
			found = true
		}
	}

	if !found {
		t.Fatalf("30d rollup lost rollup-a.test: %+v", long)
	}
}

// resetStorageState closes the database so a test can reopen it. The
// production code only ever initialises the store once.
func resetStorageState() {
	if err := CloseDB(); err != nil {
		panic(err)
	}

	windowRollupsReady.Store(false)
}
