package querylog

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"time"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/stats"
	"github.com/mia0x75/denyzer/storage"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// sharedDBPath points at the single SQLite file used by this spec file. The
// storage layer opens the embedded database only once per process, so every
// writer in this file has to be created with the same path.
var sharedDBPath string

var _ = BeforeSuite(func() {
	dir, err := os.MkdirTemp("", "denyzer-querylog-")
	Expect(err).ShouldNot(HaveOccurred())

	sharedDBPath = filepath.Join(dir, "test_async.db")
})

var _ = AfterSuite(func() {
	if sharedDBPath == "" {
		return
	}

	_ = os.RemoveAll(filepath.Dir(sharedDBPath))
})

// allTimeStats reads the all-time global counters. It returns an error instead
// of asserting so it can be used from inside an Eventually poll.
func allTimeStats() (storage.GlobalStats, error) {
	return storage.GetGlobalStats(storage.GranularityAllTime, 0)
}

// globalDelta sums the global buckets of one granularity. The buckets are
// summed because entries and failures can land in different time buckets when
// a minute or hour rolls over while the batch is assembled.
func (a *aggregationMaps) globalDelta(granularity string) *storage.MetricsDelta {
	var total storage.MetricsDelta

	found := false

	for key, delta := range a.metrics {
		if key.dimension != storage.DimensionGlobal || key.granularity != granularity {
			continue
		}

		total.Add(*delta)
		found = true
	}

	if !found {
		return nil
	}

	return &total
}

var _ = Describe("AsyncLogWriter", func() {
	var (
		ctx      context.Context
		cancelFn context.CancelFunc
	)

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)
	})

	DescribeTable("keeps latency and answered counts aligned across metric dimensions",
		func(types []string, durations []uint16, wantErrors, wantLatency, wantAverage uint64) {
			Expect(storage.InitDB(sharedDBPath)).Should(Succeed())
			now := time.Now()
			Expect(storage.MaintainWindowRollups(now)).Should(Succeed())
			agg := newAggregationMaps()
			for i, responseType := range types {
				agg.addEntry(&storage.QueryLogEntry{
					Timestamp: now.UnixMilli(), Domain: "latency.test", ClientIP: "192.0.2.1",
					QueryType: "A", ResponseType: responseType, DurationMs: durations[i],
				})
			}
			Expect(agg.metrics).Should(HaveLen(16))
			for key, delta := range agg.metrics {
				Expect(delta.Total).Should(Equal(uint64(len(types))), "%+v", key)
				Expect(delta.Errors).Should(Equal(wantErrors), "%+v", key)
				Expect(delta.TotalLatencyMs).Should(Equal(wantLatency), "%+v", key)
				stats := storage.GlobalStats{
					Total: delta.Total, Errors: delta.Errors, TotalLatencyMs: delta.TotalLatencyMs,
				}
				Expect(stats.AverageResponseMs()).Should(Equal(wantAverage), "%+v", key)
			}
			agg.addFailures(1, 1)
			delta := agg.globalDelta(storage.GranularityAllTime)
			stats := storage.GlobalStats{
				Total: delta.Total, Errors: delta.Errors, Dropped: delta.Dropped,
				TotalLatencyMs: delta.TotalLatencyMs,
			}
			Expect(stats.AverageResponseMs()).Should(Equal(wantAverage))
		},
		Entry("mixed success and BOGUS", []string{"RESOLVED", "BOGUS"}, []uint16{10, 100}, uint64(1), uint64(10), uint64(10)),
		Entry("only BOGUS", []string{"BOGUS", "BOGUS"}, []uint16{100, 200}, uint64(2), uint64(0), uint64(0)),
		Entry("successful answers", []string{"RESOLVED", "CACHED"}, []uint16{10, 20}, uint64(0), uint64(30), uint64(15)),
	)

	Describe("Write and flush", func() {
		It("excludes BOGUS latency from persisted metrics but preserves query details", func() {
			Expect(storage.InitDB(sharedDBPath)).Should(Succeed())
			before, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())
			w := &AsyncLogWriter{}
			entries := []*storage.QueryLogEntry{
				{Timestamp: time.Now().UnixMilli(), Domain: "bogus-latency.test", QueryType: "A",
					ResponseType: "RESOLVED", DurationMs: 10},
				{Timestamp: time.Now().UnixMilli(), Domain: "bogus-latency.test", QueryType: "A",
					ResponseType: "BOGUS", DurationMs: 100},
			}
			collector := stats.NewCollector()
			for _, entry := range entries {
				collector.Record(stats.Sample{
					Disposition: stats.DispositionAnswered, RType: entry.ResponseType,
					DurationMs: int64(entry.DurationMs),
				})
			}
			Expect(w.flush(entries, newAggregationMaps())).Should(Succeed())
			after, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())
			stats := storage.GlobalStats{
				Total: after.Total - before.Total, Errors: after.Errors - before.Errors,
				TotalLatencyMs: after.TotalLatencyMs - before.TotalLatencyMs,
			}
			Expect(stats.Total).Should(Equal(uint64(2)))
			Expect(stats.Errors).Should(Equal(uint64(1)))
			Expect(stats.TotalLatencyMs).Should(Equal(uint64(10)))
			Expect(stats.AverageResponseMs()).Should(Equal(uint64(10)))
			summary := collector.Snapshot().Summary
			Expect(summary.Queries).Should(Equal(int(stats.Total)))
			Expect(summary.Errors).Should(Equal(int(stats.Errors)))
			Expect(summary.AvgResponseMs).Should(Equal(int(stats.AverageResponseMs())))
			page, err := storage.GetQueryLogsPage(context.Background(), "", 10, "bogus-latency.test", "BOGUS", "", "")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(page.Entries).Should(HaveLen(1))
			Expect(page.Entries[0].DurationMs).Should(Equal(uint16(100)))
		})

		It("should write entries and flush them to the database", func() {
			w, err := NewAsyncLogWriter(ctx, sharedDBPath,
				config.QueryLogRetention{HistoryMonths: 1},
				100*time.Millisecond, 5)

			Expect(err).Should(Succeed())
			Expect(w).ShouldNot(BeNil())

			DeferCleanup(func() { _ = w.Close() })

			before, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())

			// Write 5 entries (matches batchSize, triggers immediate flush).
			for range 5 {
				w.Write(&LogEntry{
					Start:          time.Now(),
					ClientIP:       "192.168.1.1",
					ClientNames:    []string{"flush-client"},
					DurationMs:     10,
					ResponseReason: "resolved",
					ResponseType:   "RESOLVED",
					QuestionType:   "A",
					QuestionName:   "flush-example.com.",
				})
			}

			// The flush writes the query log rows and the metric buckets in
			// one atomic batch, so both become visible at the same time.
			Eventually(func() int64 {
				stats, statsErr := allTimeStats()
				if statsErr != nil {
					return -1
				}

				return int64(stats.Total) - int64(before.Total)
			}, "5s", "50ms").Should(Equal(int64(5)))

			page, err := storage.GetQueryLogsPage(context.Background(), "", 100, "", "", "flush-client", "")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(page.Entries).Should(HaveLen(5))

			// Every entry is counted as a forwarded query in the all-time
			// counters.
			after, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())
			Expect(after.Forwarded - before.Forwarded).Should(Equal(uint64(5)))
			Expect(after.Blocked - before.Blocked).Should(Equal(uint64(0)))
		})
	})

	DescribeTable("retains entries and failure counters after a failed flush",
		func(stage string) {
			Expect(storage.InitDB(sharedDBPath)).Should(Succeed())
			before, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())
			w := &AsyncLogWriter{}
			w.dropped.Add(3)
			w.failed.Add(2)
			entries := []*storage.QueryLogEntry{{
				Timestamp: time.Now().UnixMilli(), Domain: "retry-" + stage + ".test",
				QueryType: "A", ResponseType: "RESOLVED", DurationMs: 10,
			}}
			agg := newAggregationMaps()

			var conn *sql.DB
			if stage == "begin" {
				Expect(storage.CloseDB()).Should(Succeed())
				DeferCleanup(func() { Expect(storage.InitDB(sharedDBPath)).Should(Succeed()) })
			} else {
				conn, err = sql.Open("sqlite3", sharedDBPath)
				Expect(err).ShouldNot(HaveOccurred())
				DeferCleanup(func() { _ = conn.Close() })
				_, err = conn.Exec("CREATE TRIGGER fail_querylog_flush BEFORE INSERT ON " + stage +
					" BEGIN SELECT RAISE(ABORT, 'flush unavailable'); END")
				Expect(err).ShouldNot(HaveOccurred())
				DeferCleanup(func() { _, _ = conn.Exec("DROP TRIGGER IF EXISTS fail_querylog_flush") })
			}

			for range 2 {
				Expect(w.flush(entries, agg)).Should(HaveOccurred())
				Expect(w.dropped.Load()).Should(Equal(uint64(3)))
				Expect(w.failed.Load()).Should(Equal(uint64(2)))
			}
			if conn == nil {
				Expect(storage.InitDB(sharedDBPath)).Should(Succeed())
			} else {
				_, err = conn.Exec("DROP TRIGGER fail_querylog_flush")
				Expect(err).ShouldNot(HaveOccurred())
			}

			page, err := storage.GetQueryLogsPage(context.Background(), "", 10, entries[0].Domain, "", "", "")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(page.Entries).Should(BeEmpty())
			unchanged, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())
			Expect(unchanged).Should(Equal(before))

			Expect(w.flush(entries, agg)).Should(Succeed())
			Expect(w.flush(nil, agg)).Should(Succeed())
			after, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())
			Expect(after.Total - before.Total).Should(Equal(uint64(6)))
			Expect(after.Dropped - before.Dropped).Should(Equal(uint64(3)))
			Expect(after.Errors - before.Errors).Should(Equal(uint64(2)))
			Expect(after.Forwarded - before.Forwarded).Should(Equal(uint64(1)))
			page, err = storage.GetQueryLogsPage(context.Background(), "", 10, entries[0].Domain, "", "", "")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(page.Entries).Should(HaveLen(1))
		},
		Entry("batch creation", "begin"),
		Entry("query log insertion", "query_logs"),
		Entry("metric persistence", "metrics"),
	)

	Describe("Bounded retries", func() {
		var w *AsyncLogWriter
		BeforeEach(func() {
			Expect(storage.InitDB(sharedDBPath)).Should(Succeed())
			child, cancel := context.WithCancel(ctx)
			flushCtx, cancelFlush := context.WithCancel(context.Background())
			w = &AsyncLogWriter{
				ch: make(chan *storage.QueryLogEntry, 2), done: make(chan struct{}),
				batchSize: 2, flushInterval: 50 * time.Millisecond, cancel: cancel, cancelFlush: cancelFlush,
			}
			go w.consume(child, flushCtx)
			DeferCleanup(func() { _ = w.Close() })
			DeferCleanup(func() { Expect(storage.InitDB(sharedDBPath)).Should(Succeed()) })
		})

		It("retries a failed batch without consuming an unbounded queue or duplicating counters", func() {
			before, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())
			Expect(storage.CloseDB()).Should(Succeed())
			entry := &LogEntry{
				Start: time.Now(), QuestionName: "bounded-retry.test.",
				ResponseType: "RESOLVED", QuestionType: "A",
			}
			w.Write(entry)
			w.Write(entry)
			Eventually(func() int { return len(w.ch) }).Should(BeZero())
			for range 5 {
				w.Write(entry)
			}
			Expect(w.ch).Should(HaveLen(2))
			w.RecordError()
			w.RecordError()
			Expect(storage.InitDB(sharedDBPath)).Should(Succeed())

			Eventually(func() uint64 {
				stats, statsErr := allTimeStats()
				if statsErr != nil {
					return 0
				}

				return stats.Total - before.Total
			}, "5s", "10ms").Should(Equal(uint64(9)))
			Expect(w.Close()).Should(Succeed())
			page, err := storage.GetQueryLogsPage(context.Background(), "", 10, "bounded-retry.test", "", "", "")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(page.Entries).Should(HaveLen(4))
			after, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())
			Expect(after.Dropped - before.Dropped).Should(Equal(uint64(3)))
			Expect(after.Errors - before.Errors).Should(Equal(uint64(2)))
		})

		It("wakes a paused consumer on Close and reports a persistent final flush failure", func() {
			Expect(storage.CloseDB()).Should(Succeed())
			entry := &LogEntry{Start: time.Now(), QuestionName: "close-failure.test.", ResponseType: "RESOLVED"}
			w.Write(entry)
			w.Write(entry)
			Eventually(func() int { return len(w.ch) }).Should(BeZero())
			result := make(chan error, 1)
			go func() { result <- w.Close() }()
			var closeErr error
			Eventually(result, "2s").Should(Receive(&closeErr))
			Expect(closeErr).Should(MatchError(ContainSubstring("2 unwritten query log entries")))
			Expect(w.Close()).Should(MatchError(closeErr))
			Expect(w.done).Should(BeClosed())
			w.Write(entry)
			w.RecordError()
			Expect(w.ch).Should(BeEmpty())
			Expect(w.failed.Load()).Should(BeZero())
		})
	})

	Describe("Window rollups", func() {
		It("cancels a running flush waiting for the rollup lock before CloseContext returns", func() {
			w, err := NewAsyncLogWriter(ctx, sharedDBPath, config.QueryLogRetention{}, time.Hour, 1)
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(func() { _ = w.Close() })
			Expect(storage.WithWindowRollupUpdate(func() error {
				w.Write(&LogEntry{Start: time.Now(), QuestionName: "deadline-rollup.test."})
				Eventually(func() int { return len(w.ch) }).Should(BeZero())
				stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				started := time.Now()
				Expect(w.CloseContext(stopCtx)).Should(MatchError(ContainSubstring(context.DeadlineExceeded.Error())))
				Expect(time.Since(started)).Should(BeNumerically("<", 500*time.Millisecond))
				Expect(w.done).Should(BeClosed())

				return nil
			})).Should(Succeed())
			page, err := storage.GetQueryLogsPage(ctx, "", 10, "deadline-rollup.test", "", "", "")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(page.Entries).Should(BeEmpty())
		})

		It("cancels native SQLite busy waits and joins workers before CloseContext returns", func() {
			w, err := NewAsyncLogWriter(ctx, sharedDBPath, config.QueryLogRetention{}, time.Hour, 1)
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(func() { _ = w.Close() })
			conn, err := sql.Open("sqlite3", sharedDBPath)
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(func() { _ = conn.Close() })
			lock, err := conn.Conn(ctx)
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(func() { _ = lock.Close() })
			_, err = lock.ExecContext(ctx, "BEGIN IMMEDIATE")
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(func() { _, _ = lock.ExecContext(ctx, "ROLLBACK") })
			w.Write(&LogEntry{Start: time.Now(), QuestionName: "deadline-sqlite.test."})
			Eventually(func() int { return len(w.ch) }).Should(BeZero())
			stopCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			Expect(w.CloseContext(stopCtx)).Should(MatchError(ContainSubstring(context.DeadlineExceeded.Error())))
			Expect(time.Since(started)).Should(BeNumerically("<", 500*time.Millisecond))
			Expect(w.done).Should(BeClosed())
			_, err = lock.ExecContext(ctx, "ROLLBACK")
			Expect(err).ShouldNot(HaveOccurred())
			page, err := storage.GetQueryLogsPage(ctx, "", 10, "deadline-sqlite.test", "", "", "")
			Expect(err).ShouldNot(HaveOccurred())
			Expect(page.Entries).Should(BeEmpty())
		})

		It("returns an error before starting workers when initialization fails", func() {
			Expect(storage.InitDB(sharedDBPath)).Should(Succeed())
			conn, err := sql.Open("sqlite3", sharedDBPath)
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(func() { _ = conn.Close() })
			_, err = conn.Exec(`DELETE FROM metrics WHERE dimension = 'meta' AND entity = 'window_30d'`)
			Expect(err).ShouldNot(HaveOccurred())
			_, err = conn.Exec(`CREATE TRIGGER fail_window_init BEFORE INSERT ON metrics
WHEN NEW.dimension = 'meta' BEGIN SELECT RAISE(ABORT, 'window initialization failed'); END`)
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(func() { _, _ = conn.Exec(`DROP TRIGGER fail_window_init`) })
			w, err := NewAsyncLogWriter(ctx, sharedDBPath, config.QueryLogRetention{}, time.Hour, 1)
			Expect(err).Should(MatchError(ContainSubstring("initialize window rollups")))
			Expect(w).Should(BeNil())
		})

		It("waits for window maintenance before aggregating and committing a flush", func() {
			Expect(storage.InitDB(sharedDBPath)).Should(Succeed())
			Expect(storage.MaintainWindowRollups(time.Now())).Should(Succeed())
			w := &AsyncLogWriter{}
			started := make(chan struct{})
			finished := make(chan error, 1)
			Expect(storage.WithWindowRollupUpdate(func() error {
				go func() {
					close(started)
					finished <- w.flush([]*storage.QueryLogEntry{{
						Timestamp: time.Now().UnixMilli(), Domain: "exclusive-flush.test",
						ResponseType: "RESOLVED",
					}}, newAggregationMaps())
				}()
				<-started
				Consistently(finished, "50ms", "5ms").ShouldNot(Receive())

				return nil
			})).Should(Succeed())
			Eventually(finished).Should(Receive(BeNil()))
			for _, spec := range storage.WindowSpecs() {
				rows, err := storage.GetTopEntities(storage.DimensionDomain, spec.Granularity, 0, 0)
				Expect(err).ShouldNot(HaveOccurred())
				var total uint64
				for _, row := range rows {
					if row.Entity == "exclusive-flush.test" {
						total = row.Total
					}
				}
				Expect(total).Should(Equal(uint64(1)))
			}
		})

		It("should fold flushed entries into the trailing window rollups", func() {
			w, err := NewAsyncLogWriter(ctx, sharedDBPath,
				config.QueryLogRetention{HistoryMonths: 1},
				100*time.Millisecond, 5)

			Expect(err).Should(Succeed())
			DeferCleanup(func() { _ = w.Close() })

			Expect(storage.WindowRollupsReady()).Should(BeTrue())

			const domain = "rollup-spec.test"

			for range 5 {
				w.Write(&LogEntry{
					Start:          time.Now(),
					ClientIP:       "192.0.2.77",
					ClientNames:    []string{"rollup-client"},
					DurationMs:     5,
					ResponseReason: "blocked",
					ResponseType:   "BLOCKED",
					QuestionType:   "A",
					QuestionName:   domain + ".",
				})
			}

			Eventually(func() uint64 {
				entities, listErr := storage.GetTopEntities(
					storage.DimensionDomain, storage.GranularityWindow7d, 0, 0)
				if listErr != nil {
					return 0
				}

				for _, entity := range entities {
					if entity.Entity == domain {
						return entity.Total
					}
				}

				return 0
			}, "5s", "50ms").Should(Equal(uint64(5)))
		})
	})

	Describe("History cleanup", func() {
		It("removes expired history on startup", func() {
			Expect(storage.InitDB(sharedDBPath)).Should(Succeed())

			old := &storage.QueryLogEntry{
				Timestamp:    time.Now().AddDate(0, -2, 0).UnixMilli(),
				ClientIP:     "192.0.2.1",
				Domain:       "startup-cleanup-old.test",
				QueryType:    "A",
				ResponseType: "RESOLVED",
			}
			recent := &storage.QueryLogEntry{
				Timestamp:    time.Now().UnixMilli(),
				ClientIP:     "192.0.2.2",
				Domain:       "startup-cleanup-recent.test",
				QueryType:    "A",
				ResponseType: "RESOLVED",
			}
			Expect(storage.BatchInsertQueryLogs([]*storage.QueryLogEntry{old, recent})).Should(Succeed())

			w, err := NewAsyncLogWriter(ctx, sharedDBPath,
				config.QueryLogRetention{HistoryMonths: 1}, time.Hour, 1)
			Expect(err).Should(Succeed())
			DeferCleanup(func() { _ = w.Close() })

			conn, err := sql.Open("sqlite3", sharedDBPath)
			Expect(err).ShouldNot(HaveOccurred())
			DeferCleanup(func() { _ = conn.Close() })

			count := func(domain string) int {
				var n int
				if err := conn.QueryRow(
					`SELECT COUNT(*) FROM query_logs WHERE domain = ?`, domain).Scan(&n); err != nil {
					return -1
				}

				return n
			}

			Eventually(func() int { return count("startup-cleanup-old.test") }, "5s", "50ms").
				Should(BeZero())
			Expect(count("startup-cleanup-recent.test")).Should(Equal(1))
		})
	})

	Describe("Close", func() {
		It("should be safe to call Close multiple times", func() {
			w, err := NewAsyncLogWriter(ctx, sharedDBPath,
				config.QueryLogRetention{HistoryMonths: 1},
				1*time.Hour, 100)

			Expect(err).Should(Succeed())

			Expect(w.Close()).Should(Succeed())
			Expect(w.Close()).Should(Succeed()) // second call is a no-op
		})

		It("makes concurrent callers wait for the same shutdown", func() {
			child, cancel := context.WithCancel(ctx)
			w := &AsyncLogWriter{cancel: cancel, done: make(chan struct{})}
			result := make(chan error, 2)
			go func() { result <- w.Close() }()
			Eventually(child.Done()).Should(BeClosed())
			go func() { result <- w.Close() }()
			Consistently(result, "50ms").ShouldNot(Receive())
			close(w.done)
			Eventually(result).Should(Receive(BeNil()))
			Eventually(result).Should(Receive(BeNil()))
		})

		It("stops admission when the context is cancelled", func() {
			w, err := NewAsyncLogWriter(ctx, sharedDBPath, config.QueryLogRetention{}, time.Hour, 100)
			Expect(err).Should(Succeed())
			cancelFn()
			Expect(w.Close()).Should(Succeed())
			w.Write(&LogEntry{Start: time.Now(), QuestionName: "after-close.test."})
			w.RecordError()
			Expect(w.ch).Should(BeEmpty())
			Expect(w.failed.Load()).Should(BeZero())
		})
	})

	Describe("Channel full behavior", func() {
		It("should drop entries when channel is full", func() {
			w := &AsyncLogWriter{ch: make(chan *storage.QueryLogEntry, defaultChanCap)}

			// Fill the channel (cap is 2000).
			for range 2500 {
				w.Write(&LogEntry{
					Start:          time.Now(),
					ClientIP:       "192.168.1.1",
					ClientNames:    []string{"dropped-client"},
					DurationMs:     10,
					ResponseReason: "resolved",
					ResponseType:   "RESOLVED",
					QuestionType:   "A",
					QuestionName:   "dropped-example.com.",
				})
			}

			// At most 2000 entries fit into the channel, so the rest is
			// dropped instead of blocking the caller.
			Expect(w.dropped.Load()).Should(BeNumerically(">=", uint64(500)))
			Expect(w.closed).Should(BeFalse())
		})
	})

	Describe("Failure counters", func() {
		// A resolver failure cannot produce a query log entry (there is no
		// answer to log), so it is counted separately and folded into the next
		// flush alongside dropped entries.
		It("should record errors while open and close cleanly", func() {
			w, err := NewAsyncLogWriter(ctx, sharedDBPath,
				config.QueryLogRetention{HistoryMonths: 1},
				1*time.Hour, 5)
			Expect(err).Should(Succeed())

			before, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())

			w.Write(&LogEntry{
				Start:          time.Now(),
				ClientIP:       "192.168.1.2",
				ClientNames:    []string{"failure-client"},
				DurationMs:     10,
				ResponseReason: "resolved",
				ResponseType:   "RESOLVED",
				QuestionType:   "A",
				QuestionName:   "failure-example.com.",
			})

			w.RecordError()
			w.RecordError()

			Expect(w.Close()).Should(Succeed())
			DeferCleanup(func() { _ = w.Close() })

			after, err := allTimeStats()
			Expect(err).ShouldNot(HaveOccurred())

			// One answered query plus two recorded resolver failures.
			Expect(after.Total - before.Total).Should(Equal(uint64(3)))
			Expect(after.Errors - before.Errors).Should(Equal(uint64(2)))
			Expect(after.Dropped - before.Dropped).Should(Equal(uint64(0)))
			Expect(after.Forwarded - before.Forwarded).Should(Equal(uint64(1)))
		})

		It("should fold recorded failures into the error and dropped buckets", func() {
			agg := newAggregationMaps()

			agg.addEntry(&storage.QueryLogEntry{
				Timestamp:    time.Now().UnixMilli(),
				ClientIP:     "192.168.1.1",
				Domain:       "example.com",
				QueryType:    "A",
				ResponseType: "RESOLVED",
				DurationMs:   10,
			})

			// 3 dropped writes and 2 recorded resolver failures.
			agg.addFailures(3, 2)

			for _, granularity := range []string{
				storage.GranularityMinute,
				storage.GranularityHour,
				storage.GranularityDay,
				storage.GranularityAllTime,
			} {
				delta := agg.globalDelta(granularity)

				Expect(delta).ShouldNot(BeNil(), "missing global %s bucket", granularity)
				Expect(delta.Total).Should(Equal(uint64(6)))
				Expect(delta.Dropped).Should(Equal(uint64(3)))
				Expect(delta.Errors).Should(Equal(uint64(2)))
				Expect(delta.Forwarded).Should(Equal(uint64(1)))
			}
		})
	})

	Describe("Pre-aggregation correctness", func() {
		It("should aggregate entries by hour, domain and client", func() {
			w, err := NewAsyncLogWriter(ctx, sharedDBPath,
				config.QueryLogRetention{HistoryMonths: 1},
				50*time.Millisecond, 100)

			Expect(err).Should(Succeed())

			now := time.Now()
			hourBucket := storage.BucketStart(now, storage.GranularityHour)

			// Write 10 entries: 6 for agg-example.com, 4 for agg-blocked.com
			// (all in the same hour).
			for range 6 {
				w.Write(&LogEntry{
					Start:          now,
					ClientIP:       "192.168.1.10",
					ClientNames:    []string{"agg-client-1"},
					DurationMs:     10,
					ResponseReason: "resolved",
					ResponseType:   "RESOLVED",
					QuestionType:   "A",
					QuestionName:   "agg-example.com.",
				})
			}

			for range 4 {
				w.Write(&LogEntry{
					Start:          now,
					ClientIP:       "10.0.0.1",
					ClientNames:    []string{"agg-client-2"},
					DurationMs:     20,
					ResponseReason: "blocked",
					ResponseType:   "BLOCKED",
					QuestionType:   "A",
					QuestionName:   "agg-blocked.com.",
				})
			}

			// Trigger a flush via close.
			Expect(w.Close()).Should(Succeed())

			domains, err := storage.GetTopEntities(
				storage.DimensionDomain, storage.GranularityHour, hourBucket, hourBucket)
			Expect(err).ShouldNot(HaveOccurred())

			byDomain := make(map[string]storage.EntityMetric, len(domains))
			for _, entity := range domains {
				byDomain[entity.Entity] = entity
			}

			Expect(byDomain).Should(HaveKey("agg-example.com"))
			Expect(byDomain["agg-example.com"].Total).Should(Equal(uint64(6)))
			Expect(byDomain["agg-example.com"].Blocked).Should(Equal(uint64(0)))

			Expect(byDomain).Should(HaveKey("agg-blocked.com"))
			Expect(byDomain["agg-blocked.com"].Total).Should(Equal(uint64(4)))
			Expect(byDomain["agg-blocked.com"].Blocked).Should(Equal(uint64(4)))

			clients, err := storage.GetTopEntities(
				storage.DimensionClient, storage.GranularityHour, hourBucket, hourBucket)
			Expect(err).ShouldNot(HaveOccurred())

			byClient := make(map[string]storage.EntityMetric, len(clients))
			for _, entity := range clients {
				byClient[entity.Entity] = entity
			}

			Expect(byClient).Should(HaveKey("agg-client-1"))
			Expect(byClient["agg-client-1"].Total).Should(Equal(uint64(6)))

			Expect(byClient).Should(HaveKey("agg-client-2"))
			Expect(byClient["agg-client-2"].Total).Should(Equal(uint64(4)))
			Expect(byClient["agg-client-2"].Blocked).Should(Equal(uint64(4)))
		})
	})

	Describe("Retention settings", func() {
		It("should respect retention settings", func() {
			retention := config.QueryLogRetention{HistoryMonths: 7}

			Expect(retention.EffectiveHistoryMonths()).Should(Equal(7))
		})

		It("should use defaults for zero values", func() {
			retention := config.QueryLogRetention{}

			Expect(retention.EffectiveHistoryMonths()).Should(Equal(1))
		})

		It("should clamp the retention window to the supported range", func() {
			Expect(config.QueryLogRetention{HistoryMonths: 20}.EffectiveHistoryMonths()).Should(Equal(12))
			Expect(config.QueryLogRetention{HistoryMonths: -3}.EffectiveHistoryMonths()).Should(Equal(1))
		})
	})
})
