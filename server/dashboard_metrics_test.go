package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mia0x75/denyzer/storage"
)

// dashboardFixture records the metric buckets used by the dashboard tests.
type dashboardFixture struct {
	minuteBucket int64
	hourBucket   int64
	dayBucket    int64
	oldHour      int64
	oldDay       int64
}

// TestDashboardMetricsHandlers covers the metrics-backed dashboard endpoints:
// exact and independently ranked Top 50 lists, chart densities, client
// filtering, snapshot refresh and the retention cleanup.
//
// The storage layer opens the embedded database only once per process, so the
// whole test shares a single database and runs its subtests in order.
func TestDashboardMetricsHandlers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")

	if err := storage.InitDB(dbPath); err != nil {
		t.Fatalf("storage.InitDB: %v", err)
	}

	fixture := seedDashboardMetrics(t)
	invalidateDashboardCache()

	t.Run("summary is backed by the all-time metric", func(t *testing.T) {
		testDashboardSummary(t, fixture)
	})
	t.Run("top domains are ranked exactly and capped at 50", func(t *testing.T) {
		testDashboardTopDomains(t, fixture)
	})
	t.Run("top domains blocked ranking is independent", func(t *testing.T) {
		testDashboardTopDomainsBlocked(t, fixture)
	})
	t.Run("top clients are ranked from the client metric", func(t *testing.T) {
		testDashboardTopClients(t, fixture)
	})
	t.Run("chart densities match the requested range", func(t *testing.T) {
		testDashboardChartDensities(t, fixture)
	})
	t.Run("charts can be filtered by client", func(t *testing.T) {
		testDashboardChartClientFilter(t, fixture)
	})
	t.Run("query type breakdown uses the day metric", func(t *testing.T) {
		testDashboardQueryTypes(t, fixture)
	})
	t.Run("domain search matches the metric entities", func(t *testing.T) {
		testDashboardDomainSearch(t, fixture)
	})
	t.Run("refresh invalidates the cached snapshots", func(t *testing.T) {
		testDashboardRefresh(t, fixture)
	})
	t.Run("cleanup only removes expired history", func(t *testing.T) {
		testDashboardCleanup(t, fixture)
	})
	t.Run("cleanup rejects an out-of-range window", func(t *testing.T) {
		testDashboardCleanupRange(t, fixture)
	})
	t.Run("top lists are served from the window rollups", func(t *testing.T) {
		testDashboardWindowRollups(t, fixture)
	})
}

// testDashboardSummary covers the "summary is backed by the all-time metric" scenario.
func testDashboardSummary(t *testing.T, _ dashboardFixture) {
	t.Helper()

	var summary StatsSummaryResponse
	apiRequest(t, http.MethodGet, "/api/stats/summary", &summary)

	if summary.TotalQueries != 77 {
		t.Fatalf("totalQueries = %d, want 77", summary.TotalQueries)
	}

	if summary.BlockedCount != 8 {
		t.Fatalf("blocked = %d, want 8", summary.BlockedCount)
	}

	if want := float64(8) / float64(77) * 100; summary.BlockedPercentage != want {
		t.Fatalf("blockedPercentage = %v, want %v", summary.BlockedPercentage, want)
	}
}

// testDashboardTopDomains covers the "top domains are ranked exactly and capped at 50" scenario.
func testDashboardTopDomains(t *testing.T, _ dashboardFixture) {
	t.Helper()

	var all []TopDomainEntry
	rec := apiRequest(t, http.MethodGet, "/api/stats/top_domains?range=24h&limit=50", &all)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET top_domains returned %d: %s", rec.Code, rec.Body.String())
	}

	if len(all) != topSnapshotCap {
		t.Fatalf("got %d entries, want %d", len(all), topSnapshotCap)
	}

	// The ranking is exact: alpha.test (10) beats beta.test (5), which
	// beats gamma.test (1). The filler domains all tie at 1 and are
	// ordered by name.
	if all[0].Name != "alpha.test" || all[0].Count != 10 {
		t.Fatalf("all[0] = %+v, want alpha.test with count 10", all[0])
	}

	if all[1].Name != "beta.test" || all[1].Count != 5 {
		t.Fatalf("all[1] = %+v, want beta.test with count 5", all[1])
	}

	if all[2].Name != "gamma.test" || all[2].Count != 1 {
		t.Fatalf("all[2] = %+v, want gamma.test with count 1", all[2])
	}

	// Percentages use the complete window denominator (77), not the
	// ranked subset.
	if want := float64(10) / float64(77) * 100; all[0].Percentage != want {
		t.Fatalf("all[0].Percentage = %v, want %v", all[0].Percentage, want)
	}

	// zz-tail.test ranks outside of the Top 50 and must not appear.
	if containsTopName(all, "zz-tail.test") {
		t.Fatal("zz-tail.test must not be part of the Top 50 list")
	}
}

// testDashboardWindowRollups covers the "top lists are served from the window
// rollups" scenario: once the rollups are ready the 7d list reads them instead
// of the day buckets.
func testDashboardWindowRollups(t *testing.T, _ dashboardFixture) {
	t.Helper()

	now := time.Now()
	day := storage.DayBucket(now)

	seedDayDomain(t, day, "rollup-seeded.test", 4)

	if err := storage.MaintainWindowRollups(now); err != nil {
		t.Fatalf("MaintainWindowRollups: %v", err)
	}

	if !storage.WindowRollupsReady() {
		t.Fatal("window rollups are not ready after maintenance")
	}

	// Seeded after the rollup was built, so it only exists in the day buckets:
	// the 7d list must not see it once it reads the rollup.
	seedDayDomain(t, day, "day-only.test", 9)

	invalidateDashboardCache()

	var all []TopDomainEntry

	rec := apiRequest(t, http.MethodGet, "/api/stats/top_domains?range=7d&limit=50", &all)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET top_domains returned %d: %s", rec.Code, rec.Body.String())
	}

	if !containsTopName(all, "rollup-seeded.test") {
		t.Fatalf("rollup entity missing from the 7d list: %+v", all)
	}

	if containsTopName(all, "day-only.test") {
		t.Fatal("7d list read the day buckets instead of the window rollup")
	}

	// The search endpoints use the same source, so the day-only entity must
	// not show up there either.
	var found []SearchDomainEntry

	rec = apiRequest(t, http.MethodGet, "/api/stats/search_domains?range=7d&query=rollup", &found)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET search_domains returned %d: %s", rec.Code, rec.Body.String())
	}

	if len(found) != 1 || found[0].Domain != "rollup-seeded.test" || found[0].Count != 4 {
		t.Fatalf("search_domains = %+v, want the rollup entity", found)
	}

	rec = apiRequest(t, http.MethodGet, "/api/stats/search_domains?range=7d&query=day-only", &found)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET search_domains returned %d: %s", rec.Code, rec.Body.String())
	}

	if len(found) != 0 {
		t.Fatalf("search_domains read the day buckets: %+v", found)
	}
}

// seedDayDomain adds one domain/day bucket.
func seedDayDomain(t *testing.T, day int64, name string, total uint64) {
	t.Helper()

	batch, err := storage.NewBatch()
	if err != nil {
		t.Fatalf("storage.NewBatch: %v", err)
	}

	err = storage.UpsertMetrics(batch, []storage.MetricInput{{
		Dimension: storage.DimensionDomain, Granularity: storage.GranularityDay,
		BucketStart: day, Entity: name, EntityLabel: name,
		Delta: storage.MetricsDelta{Total: total},
	}})
	if err != nil {
		t.Fatalf("storage.UpsertMetrics: %v", err)
	}

	if err := storage.CommitBatch(batch); err != nil {
		t.Fatalf("storage.CommitBatch: %v", err)
	}
}

// testDashboardTopDomainsBlocked covers the "top domains blocked ranking is independent" scenario.
func testDashboardTopDomainsBlocked(t *testing.T, _ dashboardFixture) {
	t.Helper()

	var blocked []TopDomainEntry
	rec := apiRequest(t, http.MethodGet, "/api/stats/top_domains?range=24h&limit=50&blocked=true", &blocked)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET top_domains returned %d: %s", rec.Code, rec.Body.String())
	}

	if len(blocked) != 3 {
		t.Fatalf("got %d blocked entries, want 3: %+v", len(blocked), blocked)
	}

	// The blocked ranking counts blocked queries only, so beta.test (5
	// blocked) outranks alpha.test (2 blocked) even though alpha.test has
	// twice the total traffic.
	if blocked[0].Name != "beta.test" || blocked[0].Count != 5 {
		t.Fatalf("blocked[0] = %+v, want beta.test with count 5", blocked[0])
	}

	if blocked[1].Name != "alpha.test" || blocked[1].Count != 2 {
		t.Fatalf("blocked[1] = %+v, want alpha.test with count 2", blocked[1])
	}

	// The real total stays available next to the ranking count.
	if blocked[1].Total != 10 {
		t.Fatalf("blocked[1].Total = %d, want 10", blocked[1].Total)
	}

	if blocked[0].Total != 5 {
		t.Fatalf("blocked[0].Total = %d, want 5", blocked[0].Total)
	}

	// The blocked ranking is built from the complete source set, so an
	// entity outside of the Top 50 list is still ranked.
	if blocked[2].Name != "zz-tail.test" || blocked[2].Count != 1 {
		t.Fatalf("blocked[2] = %+v, want zz-tail.test with count 1", blocked[2])
	}

	// Blocked percentages use the global blocked denominator (8).
	if want := float64(5) / float64(8) * 100; blocked[0].Percentage != want {
		t.Fatalf("blocked[0].Percentage = %v, want %v", blocked[0].Percentage, want)
	}

	if want := float64(1) / float64(8) * 100; blocked[2].Percentage != want {
		t.Fatalf("blocked[2].Percentage = %v, want %v", blocked[2].Percentage, want)
	}
}

// testDashboardTopClients covers the "top clients are ranked from the client metric" scenario.
func testDashboardTopClients(t *testing.T, _ dashboardFixture) {
	t.Helper()

	var clients []TopClientEntry
	rec := apiRequest(t, http.MethodGet, "/api/stats/top_clients?range=24h&limit=50", &clients)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET top_clients returned %d: %s", rec.Code, rec.Body.String())
	}

	if len(clients) != 2 {
		t.Fatalf("got %d clients, want 2: %+v", len(clients), clients)
	}

	if clients[0].Name != "client-alpha" || clients[0].Count != 4 || clients[0].Blocked != 1 {
		t.Fatalf("clients[0] = %+v, want client-alpha with 4/1", clients[0])
	}

	if clients[1].Name != "client-beta" || clients[1].Count != 2 {
		t.Fatalf("clients[1] = %+v, want client-beta with count 2", clients[1])
	}

	if want := float64(4) / float64(77) * 100; clients[0].Percentage != want {
		t.Fatalf("clients[0].Percentage = %v, want %v", clients[0].Percentage, want)
	}
}

// testDashboardChartDensities covers the "chart densities match the requested range" scenario.
func testDashboardChartDensities(t *testing.T, _ dashboardFixture) {
	t.Helper()

	cases := []struct {
		window string
		points int
		total  uint64
	}{
		{window: "1h", points: 60, total: 10},
		{window: "24h", points: 144, total: 10},
		{window: "7d", points: 168, total: 77},
		{window: "30d", points: 720, total: 77},
	}

	for _, tc := range cases {
		var entries []TrendEntry
		rec := apiRequest(t, http.MethodGet, "/api/stats/hourly_trend?range="+tc.window, &entries)

		if rec.Code != http.StatusOK {
			t.Fatalf("GET hourly_trend returned %d: %s", rec.Code, rec.Body.String())
		}

		if len(entries) != tc.points {
			t.Fatalf("%s returned %d points, want %d", tc.window, len(entries), tc.points)
		}

		// The newest bucket carries the fixture data.
		last := entries[len(entries)-1]
		if last.Total != tc.total {
			t.Fatalf("%s last point total = %d, want %d", tc.window, last.Total, tc.total)
		}
	}
}

// testDashboardChartClientFilter covers the "charts can be filtered by client" scenario.
func testDashboardChartClientFilter(t *testing.T, _ dashboardFixture) {
	t.Helper()

	var entries []TrendEntry
	rec := apiRequest(t, http.MethodGet, "/api/stats/hourly_trend?range=1h&client=chart-client", &entries)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET hourly_trend returned %d: %s", rec.Code, rec.Body.String())
	}

	if len(entries) != 60 {
		t.Fatalf("got %d points, want 60", len(entries))
	}

	last := entries[len(entries)-1]
	if last.Total != 3 || last.Blocked != 1 {
		t.Fatalf("client point = %+v, want total 3 and blocked 1", last)
	}

	// The global bucket (10 queries) must not leak into the client chart.
	for i, entry := range entries {
		if entry.Total > 3 {
			t.Fatalf("point %d carries global data: %+v", i, entry)
		}
	}

	// An unknown client produces an empty series.
	var unknown []TrendEntry
	rec = apiRequest(t, http.MethodGet, "/api/stats/hourly_trend?range=1h&client=does-not-exist", &unknown)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET hourly_trend returned %d: %s", rec.Code, rec.Body.String())
	}

	if len(unknown) != 60 {
		t.Fatalf("got %d points, want 60", len(unknown))
	}

	for i, entry := range unknown {
		if entry.Total != 0 || entry.Blocked != 0 {
			t.Fatalf("point %d is not empty: %+v", i, entry)
		}
	}
}

// testDashboardQueryTypes covers the "query type breakdown uses the day metric" scenario.
func testDashboardQueryTypes(t *testing.T, _ dashboardFixture) {
	t.Helper()

	var entries []QueryTypeEntry
	rec := apiRequest(t, http.MethodGet, "/api/stats/query_types?days=1", &entries)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET query_types returned %d: %s", rec.Code, rec.Body.String())
	}

	if len(entries) != 2 {
		t.Fatalf("got %d query types, want 2: %+v", len(entries), entries)
	}

	if entries[0].Type != "A" || entries[0].Count != 10 {
		t.Fatalf("entries[0] = %+v, want A with count 10", entries[0])
	}

	if entries[1].Type != "AAAA" || entries[1].Count != 5 {
		t.Fatalf("entries[1] = %+v, want AAAA with count 5", entries[1])
	}

	if want := float64(10) / float64(15) * 100; entries[0].Percentage != want {
		t.Fatalf("entries[0].Percentage = %v, want %v", entries[0].Percentage, want)
	}
}

// testDashboardDomainSearch covers the "domain search matches the metric entities" scenario.
func testDashboardDomainSearch(t *testing.T, _ dashboardFixture) {
	t.Helper()

	var entries []SearchDomainEntry
	rec := apiRequest(t, http.MethodGet, "/api/stats/search_domains?range=24h&query=alpha", &entries)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET search_domains returned %d: %s", rec.Code, rec.Body.String())
	}

	if len(entries) != 1 {
		t.Fatalf("got %d domains, want 1: %+v", len(entries), entries)
	}

	if entries[0].Domain != "alpha.test" || entries[0].Count != 10 {
		t.Fatalf("entries[0] = %+v, want alpha.test with count 10", entries[0])
	}
}

// testDashboardRefresh covers the "refresh invalidates the cached snapshots" scenario.
func testDashboardRefresh(t *testing.T, fixture dashboardFixture) {
	t.Helper()

	// Prime the cache.
	var cached []TopDomainEntry
	rec := apiRequest(t, http.MethodGet, "/api/stats/top_domains?range=24h&limit=50", &cached)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET top_domains returned %d: %s", rec.Code, rec.Body.String())
	}

	if containsTopName(cached, "refreshed.test") {
		t.Fatal("refreshed.test must not exist yet")
	}

	// Add a new domain and bump the matching global counters.
	batch, err := storage.NewBatch()
	if err != nil {
		t.Fatalf("storage.NewBatch: %v", err)
	}

	inputs := []storage.MetricInput{
		{
			Dimension: storage.DimensionGlobal, Granularity: storage.GranularityHour,
			BucketStart: fixture.hourBucket, Delta: storage.MetricsDelta{Total: 3},
		},
		{
			Dimension: storage.DimensionDomain, Granularity: storage.GranularityHour,
			BucketStart: fixture.hourBucket, Entity: "refreshed.test", EntityLabel: "refreshed.test",
			Delta: storage.MetricsDelta{Total: 3},
		},
		{
			Dimension: storage.DimensionGlobal, Granularity: storage.GranularityAllTime,
			BucketStart: 0, Delta: storage.MetricsDelta{Total: 3},
		},
	}

	if err := storage.UpsertMetrics(batch, inputs); err != nil {
		t.Fatalf("storage.UpsertMetrics: %v", err)
	}

	if err := storage.CommitBatch(batch); err != nil {
		t.Fatalf("storage.CommitBatch: %v", err)
	}

	// The cached snapshot is still served until the cache is invalidated.
	var stale []TopDomainEntry
	rec = apiRequest(t, http.MethodGet, "/api/stats/top_domains?range=24h&limit=50", &stale)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET top_domains returned %d: %s", rec.Code, rec.Body.String())
	}

	if containsTopName(stale, "refreshed.test") {
		t.Fatal("the cached snapshot must not contain the new domain")
	}

	rec = apiRequest(t, http.MethodPost, "/api/stats/refresh", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST refresh returned %d: %s", rec.Code, rec.Body.String())
	}

	var fresh []TopDomainEntry
	rec = apiRequest(t, http.MethodGet, "/api/stats/top_domains?range=24h&limit=50", &fresh)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET top_domains returned %d: %s", rec.Code, rec.Body.String())
	}

	if !containsTopName(fresh, "refreshed.test") {
		t.Fatal("the refreshed snapshot must contain the new domain")
	}
}

// testDashboardCleanup covers the "cleanup only removes expired history" scenario.
func testDashboardCleanup(t *testing.T, fixture dashboardFixture) {
	t.Helper()

	var cleanup CleanupResponse
	rec := apiRequest(t, http.MethodPost, "/api/stats/cleanup?months=1", &cleanup)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST cleanup returned %d: %s", rec.Code, rec.Body.String())
	}

	if cleanup.Months != 1 {
		t.Fatalf("cleanup months = %d, want 1", cleanup.Months)
	}

	// The expired global hour bucket and the expired domain day bucket are
	// removed, the global day bucket is kept.
	if cleanup.MetricsDeleted != 2 {
		t.Fatalf("cleanup deleted %d metrics, want 2", cleanup.MetricsDeleted)
	}

	if cleanup.QueryLogsDeleted != 1 {
		t.Fatalf("cleanup deleted %d query logs, want 1", cleanup.QueryLogsDeleted)
	}

	rows, err := storage.GetMetricSeries(
		storage.DimensionGlobal, storage.GranularityHour, "", fixture.oldHour, fixture.oldHour)
	if err != nil {
		t.Fatalf("storage.GetMetricSeries: %v", err)
	}

	if len(rows) != 0 {
		t.Fatal("the expired global hour bucket survived the cleanup")
	}

	rows, err = storage.GetMetricSeries(
		storage.DimensionDomain, storage.GranularityDay, "old.test", fixture.oldDay, fixture.oldDay)
	if err != nil {
		t.Fatalf("storage.GetMetricSeries: %v", err)
	}

	if len(rows) != 0 {
		t.Fatal("the expired domain day bucket survived the cleanup")
	}

	rows, err = storage.GetMetricSeries(
		storage.DimensionGlobal, storage.GranularityDay, "", fixture.oldDay, fixture.oldDay)
	if err != nil {
		t.Fatalf("storage.GetMetricSeries: %v", err)
	}

	if len(rows) != 1 {
		t.Fatal("the global daily history must survive the cleanup")
	}

	rows, err = storage.GetMetricSeries(
		storage.DimensionGlobal, storage.GranularityHour, "", fixture.hourBucket, fixture.hourBucket)
	if err != nil {
		t.Fatalf("storage.GetMetricSeries: %v", err)
	}

	if len(rows) != 1 {
		t.Fatal("the recent global hour bucket must survive the cleanup")
	}

	// The all-time counter is preserved even though the history is gone.
	total, err := storage.GetQueryLogsTotal()
	if err != nil {
		t.Fatalf("storage.GetQueryLogsTotal: %v", err)
	}

	if total != 80 {
		t.Fatalf("all-time total after cleanup = %d, want 80", total)
	}

	page, err := storage.GetQueryLogsPage("", 100, "", "", "", "")
	if err != nil {
		t.Fatalf("storage.GetQueryLogsPage: %v", err)
	}

	if len(page.Entries) != 1 {
		t.Fatalf("got %d query log rows, want 1", len(page.Entries))
	}

	if page.Entries[0].Domain != "recent.test" {
		t.Fatalf("surviving query log = %q, want recent.test", page.Entries[0].Domain)
	}
}

// testDashboardCleanupRange covers the "cleanup rejects an out-of-range window" scenario.
func testDashboardCleanupRange(t *testing.T, _ dashboardFixture) {
	t.Helper()

	rec := apiRequest(t, http.MethodPost, "/api/stats/cleanup?months=13", nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST cleanup returned %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// seedDashboardMetrics writes the metric fixtures used by the dashboard tests.
func seedDashboardMetrics(t *testing.T) dashboardFixture {
	t.Helper()

	now := time.Now()

	fixture := dashboardFixture{
		minuteBucket: storage.BucketStart(now, storage.GranularityMinute),
		hourBucket:   storage.BucketStart(now, storage.GranularityHour),
		dayBucket:    storage.DayBucket(now),
	}

	old := now.AddDate(0, -3, 0)
	fixture.oldHour = storage.BucketStart(old, storage.GranularityHour)
	fixture.oldDay = storage.DayBucket(old)

	fixtureInputs := []storage.MetricInput{
		// Minute granularity drives the 1h and 24h charts and the 1h lists.
		{
			Dimension: storage.DimensionGlobal, Granularity: storage.GranularityMinute,
			BucketStart: fixture.minuteBucket,
			Delta: storage.MetricsDelta{
				Total: 10, Blocked: 4, Cached: 2, Forwarded: 6, TotalLatencyMs: 100,
			},
		},
		{
			Dimension: storage.DimensionDomain, Granularity: storage.GranularityMinute,
			BucketStart: fixture.minuteBucket, Entity: "minute-alpha.test", EntityLabel: "minute-alpha.test",
			Delta: storage.MetricsDelta{Total: 4, Blocked: 4},
		},
		{
			Dimension: storage.DimensionDomain, Granularity: storage.GranularityMinute,
			BucketStart: fixture.minuteBucket, Entity: "minute-beta.test", EntityLabel: "minute-beta.test",
			Delta: storage.MetricsDelta{Total: 6},
		},
		{
			Dimension: storage.DimensionClient, Granularity: storage.GranularityMinute,
			BucketStart: fixture.minuteBucket, Entity: "chart-client", EntityLabel: "chart-client",
			Delta: storage.MetricsDelta{Total: 3, Blocked: 1},
		},
		// Hour granularity drives the 24h lists and the 7d/30d charts.
		{
			Dimension: storage.DimensionGlobal, Granularity: storage.GranularityHour,
			BucketStart: fixture.hourBucket,
			Delta:       storage.MetricsDelta{Total: 77, Blocked: 8},
		},
		{
			Dimension: storage.DimensionDomain, Granularity: storage.GranularityHour,
			BucketStart: fixture.hourBucket, Entity: "alpha.test", EntityLabel: "alpha.test",
			Delta: storage.MetricsDelta{Total: 10, Blocked: 2},
		},
		{
			Dimension: storage.DimensionDomain, Granularity: storage.GranularityHour,
			BucketStart: fixture.hourBucket, Entity: "beta.test", EntityLabel: "beta.test",
			Delta: storage.MetricsDelta{Total: 5, Blocked: 5},
		},
		{
			Dimension: storage.DimensionDomain, Granularity: storage.GranularityHour,
			BucketStart: fixture.hourBucket, Entity: "gamma.test", EntityLabel: "gamma.test",
			Delta: storage.MetricsDelta{Total: 1},
		},
		{
			// zz-tail.test has the lowest traffic, so it falls outside of the
			// Top 50 list, but it is ranked in the blocked list.
			Dimension: storage.DimensionDomain, Granularity: storage.GranularityHour,
			BucketStart: fixture.hourBucket, Entity: "zz-tail.test", EntityLabel: "zz-tail.test",
			Delta: storage.MetricsDelta{Total: 1, Blocked: 1},
		},
		{
			Dimension: storage.DimensionClient, Granularity: storage.GranularityHour,
			BucketStart: fixture.hourBucket, Entity: "client-alpha", EntityLabel: "client-alpha",
			Delta: storage.MetricsDelta{Total: 4, Blocked: 1},
		},
		{
			Dimension: storage.DimensionClient, Granularity: storage.GranularityHour,
			BucketStart: fixture.hourBucket, Entity: "client-beta", EntityLabel: "client-beta",
			Delta: storage.MetricsDelta{Total: 2},
		},
		// Day granularity drives the query type breakdown and the long-term
		// history that survives a cleanup.
		{
			Dimension: storage.DimensionGlobal, Granularity: storage.GranularityDay,
			BucketStart: fixture.dayBucket,
			Delta:       storage.MetricsDelta{Total: 77, Blocked: 8},
		},
		{
			Dimension: storage.DimensionQueryType, Granularity: storage.GranularityDay,
			BucketStart: fixture.dayBucket, Entity: "A", EntityLabel: "A",
			Delta: storage.MetricsDelta{Total: 10, Blocked: 4},
		},
		{
			Dimension: storage.DimensionQueryType, Granularity: storage.GranularityDay,
			BucketStart: fixture.dayBucket, Entity: "AAAA", EntityLabel: "AAAA",
			Delta: storage.MetricsDelta{Total: 5},
		},
		// Expired history.
		{
			Dimension: storage.DimensionGlobal, Granularity: storage.GranularityHour,
			BucketStart: fixture.oldHour,
			Delta:       storage.MetricsDelta{Total: 5, Blocked: 1},
		},
		{
			Dimension: storage.DimensionGlobal, Granularity: storage.GranularityDay,
			BucketStart: fixture.oldDay,
			Delta:       storage.MetricsDelta{Total: 5, Blocked: 1},
		},
		{
			Dimension: storage.DimensionDomain, Granularity: storage.GranularityDay,
			BucketStart: fixture.oldDay, Entity: "old.test", EntityLabel: "old.test",
			Delta: storage.MetricsDelta{Total: 5, Blocked: 1},
		},
		// The all-time counter is never removed by a cleanup.
		{
			Dimension: storage.DimensionGlobal, Granularity: storage.GranularityAllTime,
			BucketStart: 0,
			Delta:       storage.MetricsDelta{Total: 77, Blocked: 8},
		},
	}

	// 60 filler domains push the source set well beyond the Top 50 cap.
	inputs := make([]storage.MetricInput, 0, len(fixtureInputs)+60)
	inputs = append(inputs, fixtureInputs...)

	for i := range 60 {
		name := fmt.Sprintf("zz-filler-%02d.test", i)

		inputs = append(inputs, storage.MetricInput{
			Dimension: storage.DimensionDomain, Granularity: storage.GranularityHour,
			BucketStart: fixture.hourBucket, Entity: name, EntityLabel: name,
			Delta: storage.MetricsDelta{Total: 1},
		})
	}

	batch, err := storage.NewBatch()
	if err != nil {
		t.Fatalf("storage.NewBatch: %v", err)
	}

	if err := storage.UpsertMetrics(batch, inputs); err != nil {
		t.Fatalf("storage.UpsertMetrics: %v", err)
	}

	if err := storage.CommitBatch(batch); err != nil {
		t.Fatalf("storage.CommitBatch: %v", err)
	}

	// One expired and one recent query log row.
	logs := []*storage.QueryLogEntry{
		{
			Timestamp: old.UnixMilli(), ClientIP: "70.0.0.1", Domain: "old.test",
			QueryType: "A", ResponseType: "BLOCKED",
		},
		{
			Timestamp: now.UnixMilli(), ClientIP: "70.0.0.2", ClientName: "recent-client",
			Domain: "recent.test", QueryType: "A", ResponseType: "RESOLVED",
		},
	}

	batch, err = storage.NewBatch()
	if err != nil {
		t.Fatalf("storage.NewBatch: %v", err)
	}

	if err := storage.BatchInsertQueryLogs(logs, batch); err != nil {
		t.Fatalf("storage.BatchInsertQueryLogs: %v", err)
	}

	if err := storage.CommitBatch(batch); err != nil {
		t.Fatalf("storage.CommitBatch: %v", err)
	}

	return fixture
}

// dashboardTestHandler wires the dashboard handlers into a test router.
func dashboardTestHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/stats/summary", func(w http.ResponseWriter, r *http.Request) {
		handleStatsSummary(w, r, func() int { return 0 })
	})
	mux.HandleFunc("/api/stats/hourly_trend", handleHourlyTrend)
	mux.HandleFunc("/api/stats/top_domains", handleTopDomains)
	mux.HandleFunc("/api/stats/top_clients", handleTopClients)
	mux.HandleFunc("/api/stats/search_domains", handleSearchDomains)
	mux.HandleFunc("/api/stats/search_clients", handleSearchClients)
	mux.HandleFunc("/api/stats/query_types", handleQueryTypesBreakdown)
	mux.HandleFunc("/api/logs/query", handleQueryLogs)
	mux.HandleFunc("/api/stats/refresh", handleRefreshStats)
	mux.HandleFunc("/api/stats/cleanup", handleCleanupHistory)

	return mux
}

// apiRequest performs a request against the dashboard handlers and decodes a
// successful JSON response into out.
func apiRequest(t *testing.T, method, path string, out any) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	rec := httptest.NewRecorder()

	dashboardTestHandler().ServeHTTP(rec, req)

	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s %s: %v", method, path, err)
		}
	}

	return rec
}

// containsTopName reports whether one of the ranked entries carries the name.
func containsTopName(entries []TopDomainEntry, name string) bool {
	for _, entry := range entries {
		if entry.Name == name {
			return true
		}
	}

	return false
}
