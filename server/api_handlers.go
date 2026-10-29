package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mia0x75/denyzer/storage"
)

const (
	defaultStatsDays   = 7
	defaultTrendHours  = 24
	defaultTopLimit    = 10
	maxQueryLogLimit   = 100
	percentageScale    = 100
	blockedFilterTrue  = "true"
	blockedFilterFalse = "false"
	minHistoryMonths   = 1
	maxHistoryMonths   = 12
)

// --- JSON response types ---

// StatsSummaryResponse is the payload for GET /api/stats/summary.
type StatsSummaryResponse struct {
	TotalQueries      uint64  `json:"totalQueries"`
	BlockedCount      uint64  `json:"blocked"`
	BlockedPercentage float64 `json:"blockedPercentage"`
	CachedCount       uint64  `json:"cached"`
	ForwardedCount    uint64  `json:"forwarded"`
	FilteredCount     uint64  `json:"filtered"`
	LocalCount        uint64  `json:"local"`
	ErrorCount        uint64  `json:"errors"`
	OtherCount        uint64  `json:"other"`
	DroppedCount      uint64  `json:"dropped"`
	CacheHitRate      float64 `json:"cacheHitRate"`
	AvgResponseMs     uint64  `json:"avgResponseMs"`
	CacheEntries      uint64  `json:"cacheEntries"`
}

// TrendEntry is one data point in a dashboard trend array.
type TrendEntry struct {
	Time           string `json:"time"`
	Total          uint64 `json:"total"`
	Blocked        uint64 `json:"blocked"`
	Cached         uint64 `json:"cached"`
	AverageLatency uint16 `json:"avgResponseMs"`
}

// TopDomainEntry is one item in the top domains list. Count is the ranking
// value for the requested filter; Total and Blocked expose both counters so the
// UI can render the split without another request.
type TopDomainEntry struct {
	Name       string  `json:"name"`
	Count      uint64  `json:"count"`
	Total      uint64  `json:"total"`
	Blocked    uint64  `json:"blocked"`
	Percentage float64 `json:"percentage"`
}

// TopClientEntry is one item in the top clients list.
type TopClientEntry struct {
	Name       string  `json:"name"`
	Count      uint64  `json:"count"`
	Total      uint64  `json:"total"`
	Blocked    uint64  `json:"blocked"`
	Percentage float64 `json:"percentage"`
}

// QueryLogResponse is the payload for GET /api/logs/query.
type QueryLogResponse struct {
	Items      []*QueryLogItem `json:"items"`
	NextCursor string          `json:"nextCursor,omitempty"`
	HasMore    bool            `json:"hasMore"`
	TotalCount int64           `json:"totalCount,omitempty"`
}

// QueryLogItem is a single query log entry in the API response.
type QueryLogItem struct {
	ID           int    `json:"id"`
	RequestTs    string `json:"requestTs"`
	ClientIP     string `json:"clientIp"`
	ClientName   string `json:"clientName"`
	DurationMs   uint16 `json:"durationMs"`
	Reason       string `json:"reason"`
	QuestionName string `json:"questionName"`
	ResponseType string `json:"responseType"`
	QuestionType string `json:"questionType"`
}

// SearchDomainEntry is one item in the domain search results.
type SearchDomainEntry struct {
	Domain string `json:"domain"`
	Count  uint64 `json:"count"`
}

// SearchClientEntry is one item in the client search results.
type SearchClientEntry struct {
	Client string `json:"client"`
	Count  uint64 `json:"count"`
}

// QueryTypeEntry is one item in the query types breakdown.
type QueryTypeEntry struct {
	Type       string  `json:"type"`
	Count      uint64  `json:"count"`
	Percentage float64 `json:"percentage"`
}

// ListedStatsResponse is the payload for GET /api/stats/lists.
type ListedStatsResponse struct {
	DenylistGroups   int `json:"denylistGroups"`
	ListedDomains    int `json:"listedDomains"`
	AllowlistDomains int `json:"allowlistDomains"`
}

// CleanupResponse reports the rows removed by a normal history cleanup.
type CleanupResponse struct {
	Months           int `json:"months"`
	QueryLogsDeleted int `json:"queryLogsDeleted"`
	MetricsDeleted   int `json:"metricsDeleted"`
}

// --- Utility helpers ---

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	// error response; the status line is already written, so an encode failure
	// cannot be surfaced to the client.
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errchkjson
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}

	return n
}

func queryString(r *http.Request, key string) string {
	return r.URL.Query().Get(key)
}

// --- HTTP Handlers ---

// handleStatsSummary serves GET /api/stats/summary. Totals are all-time,
// cumulative counters read from the global all_time metric bucket.
func handleStatsSummary(w http.ResponseWriter, _ *http.Request, cacheEntryCount func() int) {
	stats, err := dashboardCache.getSummary()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("can't read metrics: %v", err))

		return
	}

	var blockedPct float64
	if stats.Total > 0 {
		blockedPct = float64(stats.Blocked) / float64(stats.Total) * percentageScale
	}

	writeJSON(w, StatsSummaryResponse{
		TotalQueries:      stats.Total,
		BlockedCount:      stats.Blocked,
		BlockedPercentage: blockedPct,
		CachedCount:       stats.Cached,
		ForwardedCount:    stats.Forwarded,
		FilteredCount:     stats.Filtered,
		LocalCount:        stats.Local,
		ErrorCount:        stats.Errors,
		OtherCount:        stats.Other,
		DroppedCount:      stats.Dropped,
		CacheHitRate:      stats.CacheHitRate(),
		AvgResponseMs:     stats.AverageResponseMs(),
		CacheEntries:      resolveCacheEntryCount(cacheEntryCount),
	})
}

// resolveCacheEntryCount resolves the supplied cache-entry counter, treating a
// nil provider as an unavailable cache (0 entries).
func resolveCacheEntryCount(cacheEntryCount func() int) uint64 {
	if cacheEntryCount == nil {
		return 0
	}

	count := cacheEntryCount()
	if count <= 0 {
		return 0
	}

	return uint64(count)
}

// trendSpec describes the chart resolution requested by a dashboard range.
type trendSpec struct {
	granularity string
	step        time.Duration
	points      int
}

// Chart densities of the dashboard ranges.
const (
	trendPointsOneHour   = 60
	trendPoints24Hours   = 144
	trendPoints7Days     = 168
	trendPoints30Days    = 720
	trendStep24Hours     = 10 * time.Minute
	trendGranularityHour = storage.GranularityHour
)

func trendSpecFor(window string) trendSpec {
	switch window {
	case windowOneHour:
		return trendSpec{
			granularity: storage.GranularityMinute,
			step:        time.Minute,
			points:      trendPointsOneHour,
		}
	case window7Days:
		return trendSpec{
			granularity: trendGranularityHour,
			step:        time.Hour,
			points:      trendPoints7Days,
		}
	case window30Days:
		return trendSpec{
			granularity: trendGranularityHour,
			step:        time.Hour,
			points:      trendPoints30Days,
		}
	default: // 24h
		return trendSpec{
			granularity: storage.GranularityMinute,
			step:        trendStep24Hours,
			points:      trendPoints24Hours,
		}
	}
}

// handleHourlyTrend serves GET /api/stats/hourly_trend?range=24h&client=...
// Range controls both the window and the chart density:
//   - 1h: 60 one-minute points
//   - 24h: 144 ten-minute points
//   - 7d: 168 hourly points
//   - 30d: 720 hourly points
func handleHourlyTrend(w http.ResponseWriter, r *http.Request) {
	window := normalizeWindow(
		queryString(r, "range"),
		queryInt(r, "days", 0),
		queryInt(r, "hours", defaultTrendHours),
	)

	dimension := storage.DimensionGlobal
	entity := ""

	if client := queryString(r, "client"); client != "" {
		dimension = storage.DimensionClient
		entity = client
	}

	entries, err := buildTrend(dimension, entity, trendSpecFor(window))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("can't read trend metrics: %v", err))

		return
	}

	writeJSON(w, entries)
}

func buildTrend(dimension, entity string, spec trendSpec) ([]TrendEntry, error) {
	now := time.Now()
	axisEnd := now.Truncate(spec.step)
	axisStart := axisEnd.Add(-time.Duration(spec.points-1) * spec.step)

	sourceEnd := now.Truncate(time.Minute)
	if spec.granularity == storage.GranularityHour {
		sourceEnd = now.Truncate(time.Hour)
	}

	sourceStart := axisStart.Truncate(sourceGranularity(spec.granularity))

	rows, err := storage.GetMetricSeries(
		dimension,
		spec.granularity,
		entity,
		sourceStart.Unix(),
		sourceEnd.Unix(),
	)
	if err != nil {
		return nil, err
	}

	byBucket := make(map[int64]storage.GlobalStats, spec.points)

	for _, row := range rows {
		bucket := time.Unix(row.BucketStart, 0).Truncate(spec.step).Unix()
		stats := byBucket[bucket]

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

		byBucket[bucket] = stats
	}

	result := make([]TrendEntry, 0, spec.points)

	for i := range spec.points {
		bucketTime := axisStart.Add(time.Duration(i) * spec.step)
		stats := byBucket[bucketTime.Unix()]

		result = append(result, TrendEntry{
			Time:           bucketTime.Format(time.RFC3339),
			Total:          stats.Total,
			Blocked:        stats.Blocked,
			Cached:         stats.Cached,
			AverageLatency: clampLatency(stats.AverageResponseMs()),
		})
	}

	return result, nil
}

func sourceGranularity(granularity string) time.Duration {
	if granularity == storage.GranularityHour {
		return time.Hour
	}

	return time.Minute
}

// clampLatency clamps an average response time to the uint16 API field.
func clampLatency(avgMs uint64) uint16 {
	const maxUint16 = 65535

	if avgMs > maxUint16 {
		return maxUint16
	}

	return uint16(avgMs)
}

// topEntry is the shared shape of the top list response items.
type topEntry interface {
	TopDomainEntry | TopClientEntry
}

// handleTopDomains serves GET /api/stats/top_domains?range=24h&limit=50&blocked=true.
// All and blocked rankings are cached independently and each percentage uses the
// complete window denominator.
func handleTopDomains(w http.ResponseWriter, r *http.Request) {
	handleTopEntities[TopDomainEntry](w, r, storage.DimensionDomain, "domains")
}

// handleTopClients serves GET /api/stats/top_clients?range=24h&limit=50&blocked=true.
func handleTopClients(w http.ResponseWriter, r *http.Request) {
	handleTopEntities[TopClientEntry](w, r, storage.DimensionClient, "clients")
}

// handleTopEntities serves a top list of the requested dimension. The All and
// the blocked ranking are cached independently, the percentage of an entry is
// relative to the complete window.
func handleTopEntities[T topEntry](w http.ResponseWriter, r *http.Request, dimension, label string) {
	window := normalizeWindow(
		queryString(r, "range"),
		queryInt(r, "days", 0),
		queryInt(r, "hours", 0),
	)
	limit := clampTopLimit(queryInt(r, "limit", topSnapshotCap))
	blocked := queryString(r, "blocked") == blockedFilterTrue

	snapshot, err := dashboardCache.getTop(dimension, window)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("can't read top %s: %v", label, err))

		return
	}

	source := snapshot.All
	if blocked {
		source = snapshot.Blocked
	}

	result := make([]T, 0, min(limit, len(source)))

	for _, item := range source {
		if len(result) >= limit {
			break
		}

		result = append(result, T(item))
	}

	writeJSON(w, result)
}

func clampTopLimit(limit int) int {
	if limit <= 0 {
		return topSnapshotCap
	}

	return min(limit, topSnapshotCap)
}

// handleQueryLogs serves
// GET /api/logs/query?limit=100&cursor=&search=&responseType=&client=&questionType=
func handleQueryLogs(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", maxQueryLogLimit)
	cursor := queryString(r, "cursor")
	search := queryString(r, "search")
	responseType := queryString(r, "responseType")
	client := queryString(r, "client")
	questionType := queryString(r, "questionType")

	if limit <= 0 {
		limit = maxQueryLogLimit
	}

	if limit > maxQueryLogLimit {
		limit = maxQueryLogLimit
	}

	page, err := storage.GetQueryLogsPage(cursor, limit, search, responseType, client, questionType)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("can't read query_logs: %v", err))

		return
	}

	items := make([]*QueryLogItem, 0, len(page.Entries))

	for _, e := range page.Entries {
		ts := time.UnixMilli(e.Timestamp).UTC().Format(time.RFC3339)

		items = append(items, &QueryLogItem{
			ID:           e.ID,
			RequestTs:    ts,
			ClientIP:     e.ClientIP,
			ClientName:   e.ClientName,
			DurationMs:   e.DurationMs,
			Reason:       e.Reason,
			QuestionName: e.Domain,
			ResponseType: e.ResponseType,
			QuestionType: e.QueryType,
		})
	}

	response := QueryLogResponse{
		Items:      items,
		NextCursor: page.NextCursor,
		HasMore:    page.HasMore,
	}

	// The exact total is only cheap (and meaningful) for the unfiltered list.
	if search == "" && responseType == "" && client == "" && questionType == "" {
		total, totalErr := storage.GetQueryLogsTotal()
		if totalErr != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Sprintf("can't read query_logs total: %v", totalErr))

			return
		}

		response.TotalCount = total
	}

	writeJSON(w, response)
}

// handleSearchDomains serves GET /api/stats/search_domains?range=24h&query=&limit=10
func handleSearchDomains(w http.ResponseWriter, r *http.Request) {
	window := windowFromRequest(r)
	limit := clampTopLimit(queryInt(r, "limit", defaultTopLimit))
	query := queryString(r, "query")

	if query == "" {
		writeJSON(w, []SearchDomainEntry{})

		return
	}

	granularity, start, end := topEntityBounds(window)

	entities, err := storage.GetTopEntities(storage.DimensionDomain, granularity, start, end)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("can't read domain metrics: %v", err))

		return
	}

	queryLower := strings.ToLower(query)
	result := make([]SearchDomainEntry, 0)

	for _, entity := range entities {
		if !strings.Contains(strings.ToLower(entity.Entity), queryLower) {
			continue
		}

		result = append(result, SearchDomainEntry{
			Domain: entity.Entity,
			Count:  entity.Total,
		})
	}

	slices.SortFunc(result, func(a, b SearchDomainEntry) int {
		if a.Count > b.Count {
			return -1
		}

		if a.Count < b.Count {
			return 1
		}

		return cmpStrings(a.Domain, b.Domain)
	})

	if limit < len(result) {
		result = result[:limit]
	}

	writeJSON(w, result)
}

// handleSearchClients serves GET /api/stats/search_clients?range=24h&query=&limit=10
func handleSearchClients(w http.ResponseWriter, r *http.Request) {
	window := windowFromRequest(r)
	limit := clampTopLimit(queryInt(r, "limit", defaultTopLimit))
	query := queryString(r, "query")

	if query == "" {
		writeJSON(w, []SearchClientEntry{})

		return
	}

	granularity, start, end := topEntityBounds(window)

	entities, err := storage.GetTopEntities(storage.DimensionClient, granularity, start, end)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("can't read client metrics: %v", err))

		return
	}

	queryLower := strings.ToLower(query)
	result := make([]SearchClientEntry, 0)

	for _, entity := range entities {
		label := entity.Label
		if label == "" {
			label = entity.Entity
		}

		if !strings.Contains(strings.ToLower(entity.Entity), queryLower) &&
			!strings.Contains(strings.ToLower(label), queryLower) {
			continue
		}

		result = append(result, SearchClientEntry{
			Client: label,
			Count:  entity.Total,
		})
	}

	slices.SortFunc(result, func(a, b SearchClientEntry) int {
		if a.Count > b.Count {
			return -1
		}

		if a.Count < b.Count {
			return 1
		}

		return cmpStrings(a.Client, b.Client)
	})

	if limit < len(result) {
		result = result[:limit]
	}

	writeJSON(w, result)
}

func cmpStrings(a, b string) int {
	if a < b {
		return -1
	}

	if a > b {
		return 1
	}

	return 0
}

func windowFromRequest(r *http.Request) string {
	window := queryString(r, "range")
	if window != "" {
		return normalizeWindow(window, 0, 0)
	}

	return normalizeWindow("", queryInt(r, "days", 0), 0)
}

// handleQueryTypesBreakdown serves GET /api/stats/query_types?days=7
func handleQueryTypesBreakdown(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", defaultStatsDays)

	var (
		counts map[string]uint64
		err    error
	)

	if days <= 0 {
		counts, err = storage.GetQueryTypeCounts(storage.GranularityAllTime, 0, 0)
	} else {
		now := time.Now()
		start := storage.DayBucket(now.AddDate(0, 0, -(days - 1)))
		end := storage.DayBucket(now)
		counts, err = storage.GetQueryTypeCounts(storage.GranularityDay, start, end)
	}

	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("can't read query type metrics: %v", err))

		return
	}

	var total uint64

	for _, count := range counts {
		total += count
	}

	result := make([]QueryTypeEntry, 0, len(counts))

	for qt, count := range counts {
		var pct float64
		if total > 0 {
			pct = float64(count) / float64(total) * percentageScale
		}

		result = append(result, QueryTypeEntry{
			Type:       qt,
			Count:      count,
			Percentage: pct,
		})
	}

	slices.SortFunc(result, func(a, b QueryTypeEntry) int {
		if a.Count > b.Count {
			return -1
		}

		if a.Count < b.Count {
			return 1
		}

		return cmpStrings(a.Type, b.Type)
	})

	writeJSON(w, result)
}

// handleRefreshStats serves POST /api/stats/refresh. It invalidates cached
// dashboard snapshots; all_time metrics are already authoritative, so no raw
// log recalculation is needed.
func handleRefreshStats(w http.ResponseWriter, _ *http.Request) {
	invalidateDashboardCache()
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleCleanupHistory serves POST /api/stats/cleanup?months=1. Normal cleanup
// only removes rows older than the cutoff and preserves all-time counters plus
// global daily history.
func handleCleanupHistory(w http.ResponseWriter, r *http.Request) {
	months := queryInt(r, "months", minHistoryMonths)
	if months < minHistoryMonths || months > maxHistoryMonths {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("months must be between %d and %d",
			minHistoryMonths, maxHistoryMonths))

		return
	}

	result, err := storage.CleanUp(months)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("can't clean up history: %v", err))

		return
	}

	invalidateDashboardCache()

	writeJSON(w, CleanupResponse{
		Months:           months,
		QueryLogsDeleted: result.QueryLogsDeleted,
		MetricsDeleted:   result.MetricsDeleted,
	})
}

// handleStatsListed serves GET /api/stats/lists with the current denylist and
// allowlist entry counts.
func handleStatsListed(w http.ResponseWriter, provider listCountsProvider) {
	if provider == nil {
		writeErr(w, http.StatusServiceUnavailable, "statistics are disabled")

		return
	}

	denylist, allowlist := provider.ListCounts()

	var listedDomains int

	for _, count := range denylist {
		listedDomains += count
	}

	var allowlistDomains int

	for _, count := range allowlist {
		allowlistDomains += count
	}

	writeJSON(w, ListedStatsResponse{
		DenylistGroups:   len(denylist),
		ListedDomains:    listedDomains,
		AllowlistDomains: allowlistDomains,
	})
}

// Keep these constants referenced by future filters/documentation without
// changing the public query contract.
var _ = []string{blockedFilterFalse}
