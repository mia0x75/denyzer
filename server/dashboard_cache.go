package server

import (
	"cmp"
	"slices"
	"sync"
	"time"

	"github.com/mia0x75/denyzer/storage"
)

const (
	snapshotTTL    = 30 * time.Second
	topSnapshotCap = 50
)

// Dashboard windows. The window selects both the metric granularity and the
// chart density.
const (
	windowOneHour = "1h"
	window24Hours = "24h"
	window7Days   = "7d"
	window30Days  = "30d"
)

// Day thresholds used when a request asks for a number of days instead of a
// named window.
const (
	sevenDayWindowDays  = 7
	thirtyDayWindowDays = 30
)

type topEntitySnapshot struct {
	Name       string
	Count      uint64
	Total      uint64
	Blocked    uint64
	Percentage float64
}

type topWindowSnapshot struct {
	All          []topEntitySnapshot
	Blocked      []topEntitySnapshot
	Total        uint64
	BlockedTotal uint64
	Expires      time.Time
}

type summarySnapshot struct {
	Stats   storage.GlobalStats
	Expires time.Time
}

// dashboardSnapshotCache keeps the dashboard snapshots.

// The mutex only guards the maps and the in-flight bookkeeping; a snapshot is
// always built outside it so that a slow rebuild cannot block the other cards.
type dashboardSnapshotCache struct {
	mu       sync.Mutex
	domains  map[string]*topWindowSnapshot
	clients  map[string]*topWindowSnapshot
	summary  *summarySnapshot
	building map[string]chan struct{}
}

//nolint:gochecknoglobals
var dashboardCache = &dashboardSnapshotCache{
	domains:  make(map[string]*topWindowSnapshot),
	clients:  make(map[string]*topWindowSnapshot),
	building: make(map[string]chan struct{}),
}

// invalidateDashboardCache drops every cached dashboard snapshot.
func invalidateDashboardCache() {
	dashboardCache.mu.Lock()
	defer dashboardCache.mu.Unlock()

	clear(dashboardCache.domains)
	clear(dashboardCache.clients)

	dashboardCache.summary = nil
}

func (c *dashboardSnapshotCache) getSummary() (storage.GlobalStats, error) {
	c.mu.Lock()
	now := time.Now()
	if c.summary != nil && now.Before(c.summary.Expires) {
		stats := c.summary.Stats
		c.mu.Unlock()

		return stats, nil
	}
	c.mu.Unlock()

	stats, err := storage.GetGlobalStats(storage.GranularityAllTime, 0)
	if err != nil {
		return storage.GlobalStats{}, err
	}

	c.mu.Lock()
	c.summary = &summarySnapshot{Stats: stats, Expires: now.Add(snapshotTTL)}
	c.mu.Unlock()

	return stats, nil
}

// getTop returns the cached, independently ranked Top 50 snapshots for a window.
//
// A cached snapshot is served until it expires. Once it expires the stale
// snapshot is still returned immediately while a single background rebuild
// refreshes it, so a slow 30-day rebuild never stalls the dashboard. A cold
// window (no snapshot yet) is built synchronously, and concurrent callers wait
// for that first build instead of starting their own.
func (c *dashboardSnapshotCache) getTop(dimension, window string) (*topWindowSnapshot, error) {
	cache, key := c.topCache(dimension), dimension+":"+window

	for {
		c.mu.Lock()

		snapshot, ok := cache[window]
		if ok && time.Now().Before(snapshot.Expires) {
			c.mu.Unlock()

			return snapshot, nil
		}

		if wait, inflight := c.building[key]; inflight {
			c.mu.Unlock()

			if ok {
				return snapshot, nil
			}

			<-wait

			continue
		}

		done := make(chan struct{})
		c.building[key] = done

		c.mu.Unlock()

		if ok {
			go func() {
				defer c.finishBuild(key, done)
				_, _ = c.buildTop(dimension, window, cache)
			}()

			return snapshot, nil
		}

		snapshot, err := c.buildTop(dimension, window, cache)
		c.finishBuild(key, done)

		if err != nil {
			return nil, err
		}

		return snapshot, nil
	}
}

func (c *dashboardSnapshotCache) topCache(dimension string) map[string]*topWindowSnapshot {
	if dimension == storage.DimensionClient {
		return c.clients
	}

	return c.domains
}

// buildTop rebuilds one snapshot and stores it in the cache.
func (c *dashboardSnapshotCache) buildTop(
	dimension, window string, cache map[string]*topWindowSnapshot,
) (*topWindowSnapshot, error) {
	snapshot, err := buildTopSnapshot(dimension, window)
	if err != nil {
		return nil, err
	}

	snapshot.Expires = time.Now().Add(snapshotTTL)

	c.mu.Lock()
	cache[window] = snapshot
	c.mu.Unlock()

	return snapshot, nil
}

// finishBuild clears the in-flight marker and wakes the waiting callers.
func (c *dashboardSnapshotCache) finishBuild(key string, done chan struct{}) {
	c.mu.Lock()
	delete(c.building, key)
	c.mu.Unlock()

	close(done)
}

func buildTopSnapshot(dimension, window string) (*topWindowSnapshot, error) {
	entityGranularity, entityStart, entityEnd := topEntityBounds(window)

	entities, err := storage.GetTopEntities(dimension, entityGranularity, entityStart, entityEnd)
	if err != nil {
		return nil, err
	}

	// The percentages always use the complete window as the denominator, even
	// when the entities themselves come from the trailing rollup.
	totalGranularity, totalStart, totalEnd := topWindowBounds(window)

	global, err := storage.SumGlobalStats(totalGranularity, totalStart, totalEnd)
	if err != nil {
		return nil, err
	}

	all, blocked := rankTopEntities(entities)

	applyTopPercentages(all, global.Total)
	applyTopPercentages(blocked, global.Blocked)

	return &topWindowSnapshot{
		All:          all,
		Blocked:      blocked,
		Total:        global.Total,
		BlockedTotal: global.Blocked,
	}, nil
}

// rankTopEntities builds the All and the blocked ranking. Both rankings are
// independent: the blocked ranking only contains the entities with at least
// one blocked query and keeps the real total of the entity next to it.
func rankTopEntities(entities []storage.EntityMetric) (all, blocked []topEntitySnapshot) {
	all = make([]topEntitySnapshot, 0, len(entities))
	blocked = make([]topEntitySnapshot, 0, len(entities))

	for _, entity := range entities {
		name := entity.Label
		if name == "" {
			name = entity.Entity
		}

		all = append(all, topEntitySnapshot{
			Name:    name,
			Count:   entity.Total,
			Total:   entity.Total,
			Blocked: entity.Blocked,
		})

		if entity.Blocked > 0 {
			blocked = append(blocked, topEntitySnapshot{
				Name:    name,
				Count:   entity.Blocked,
				Total:   entity.Total,
				Blocked: entity.Blocked,
			})
		}
	}

	sortTopEntities(all)
	sortTopEntities(blocked)

	return truncateTopEntities(all), truncateTopEntities(blocked)
}

func sortTopEntities(items []topEntitySnapshot) {
	slices.SortFunc(items, func(a, b topEntitySnapshot) int {
		if byCount := cmp.Compare(b.Count, a.Count); byCount != 0 {
			return byCount
		}

		return cmp.Compare(a.Name, b.Name)
	})
}

// truncateTopEntities keeps the Top 50 of a ranking. The source buckets are
// never pruned by rank, the truncation only limits the returned list.
func truncateTopEntities(items []topEntitySnapshot) []topEntitySnapshot {
	if len(items) > topSnapshotCap {
		return items[:topSnapshotCap]
	}

	return items
}

// applyTopPercentages sets the percentage of every entry relative to the
// complete window, not relative to the ranked subset.
func applyTopPercentages(items []topEntitySnapshot, total uint64) {
	if total == 0 {
		return
	}

	for i := range items {
		items[i].Percentage = float64(items[i].Count) / float64(total) * percentageScale
	}
}

// topWindowBounds maps a dashboard window to the metrics granularity and
// inclusive bucket range that hold the window totals. Sub-day windows use
// minute/hour buckets; long windows use daily buckets. The returned
// percentages are always based on the full window, not on the ranked subset.
func topWindowBounds(window string) (granularity string, start, end int64) {
	now := time.Now()

	switch window {
	case windowOneHour:
		endTime := now.Truncate(time.Minute)
		startTime := endTime.Add(-59 * time.Minute)

		return storage.GranularityMinute, startTime.Unix(), endTime.Unix()
	case window7Days:
		return dailyWindowBounds(now, sevenDayWindowDays)
	case window30Days:
		return dailyWindowBounds(now, thirtyDayWindowDays)
	default: // 24h
		endTime := now.Truncate(time.Hour)
		startTime := endTime.Add(-23 * time.Hour)

		return storage.GranularityHour, startTime.Unix(), endTime.Unix()
	}
}

func dailyWindowBounds(now time.Time, days int) (granularity string, start, end int64) {
	return storage.GranularityDay, storage.WindowStart(now, days), storage.DayBucket(now)
}

// topEntityBounds returns where the ranked entities come from. The 7d and 30d
// windows read the trailing window rollups (one row per entity) instead of
// every day bucket. Until the rollups are ready, the day buckets are used.
func topEntityBounds(window string) (granularity string, start, end int64) {
	if storage.WindowRollupsReady() {
		switch window {
		case window7Days:
			return storage.GranularityWindow7d, 0, 0
		case window30Days:
			return storage.GranularityWindow30d, 0, 0
		}
	}

	return topWindowBounds(window)
}

func normalizeWindow(window string, days, hours int) string {
	switch window {
	case windowOneHour, window24Hours, window7Days, window30Days:
		return window
	}

	if days >= thirtyDayWindowDays {
		return window30Days
	}

	if days >= sevenDayWindowDays {
		return window7Days
	}

	if hours == 1 {
		return windowOneHour
	}

	return window24Hours
}
