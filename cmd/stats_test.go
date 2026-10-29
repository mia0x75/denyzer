package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func sampleSummary() statsSummary {
	return statsSummary{
		TotalQueries: 12431, CachedCount: 5102, ForwardedCount: 4445,
		BlockedCount: 2884, BlockedPercentage: 23.2, FilteredCount: 1620,
		LocalCount: 12, DroppedCount: 3, ErrorCount: 7,
		AvgResponseMs: 7, CacheHitRate: 0.41, CacheEntries: 8123,
	}
}

func sampleStats() *statsSnapshot {
	return &statsSnapshot{
		summary:   new(sampleSummary()),
		topDomain: []nameCount{{Name: "github.com", Count: 1203}},
		topBlkDom: []nameCount{{Name: "ads.example.com", Count: 402}},
		topClient: []clientCount{{Name: "10.0.0.5", Count: 4310, Blocked: 1204}},
		queryType: []queryTypeCount{
			{Type: "A", Count: 9000, Percentage: 72.4},
			{Type: "AAAA", Count: 3431, Percentage: 27.6},
		},
		lists: &statsLists{DenylistGroups: 2, ListedDomains: 147000, AllowlistDomains: 30},
		days:  7,
	}
}

var _ = Describe("renderStats", func() {
	It("renders a populated snapshot with formatted numbers and all sections", func() {
		var buf bytes.Buffer

		renderStats(&buf, sampleStats())
		out := buf.String()

		Expect(out).Should(ContainSubstring("Window:"))
		Expect(out).Should(ContainSubstring("(UTC)"))
		Expect(out).Should(ContainSubstring("last 7 day(s)"))
		Expect(out).Should(ContainSubstring("Summary (all time)"))
		Expect(out).Should(ContainSubstring("Queries"))
		Expect(out).Should(ContainSubstring("12,431")) // thousands separator
		Expect(out).Should(ContainSubstring("41.0%"))  // cache hit rate 0.41 -> 41.0%
		Expect(out).Should(ContainSubstring("Top Domains"))
		Expect(out).Should(ContainSubstring("github.com"))
		Expect(out).Should(ContainSubstring("Top Blocked"))
		Expect(out).Should(ContainSubstring("ads.example.com"))
		// blocked and filtered are separate rows, each with its own share of queries
		Expect(out).Should(ContainSubstring("Blocked"))
		Expect(out).Should(ContainSubstring("2,884 (23.2%)"))
		Expect(out).Should(ContainSubstring("Filtered"))
		Expect(out).Should(ContainSubstring("1,620 (13.0%)"))
		Expect(out).Should(ContainSubstring("Top Clients"))
		Expect(out).Should(ContainSubstring("10.0.0.5"))
		Expect(out).Should(ContainSubstring("1,204"))
		Expect(out).Should(ContainSubstring("By Query Type"))
		Expect(out).Should(ContainSubstring("Cache: 8,123 entries"))
		Expect(out).Should(ContainSubstring("Lists"))
		Expect(out).Should(ContainSubstring("7 ms"))
		Expect(out).Should(ContainSubstring("147,000"))
		// higher count sorts first
		Expect(strings.Index(out, "9,000")).Should(BeNumerically("<", strings.Index(out, "3,431")))
	})

	It("renders (none) for empty lists and avoids divide-by-zero when there are no queries", func() {
		s := &statsSnapshot{
			summary: &statsSummary{}, // all zero
			days:    7,
		}
		var buf bytes.Buffer

		renderStats(&buf, s)
		out := buf.String()

		Expect(out).Should(ContainSubstring("Top Domains: (none)"))
		Expect(out).Should(ContainSubstring("Top Blocked: (none)"))
		Expect(out).Should(ContainSubstring("Top Clients: (none)"))
		Expect(out).Should(ContainSubstring("By Query Type: (none)"))
		Expect(out).Should(ContainSubstring("Lists: (none)"))
		Expect(out).Should(ContainSubstring("0.0%")) // no panic, graceful percentage
	})

	It("formats integers with thousands separators", func() {
		Expect(formatInt(0)).Should(Equal("0"))
		Expect(formatInt(999)).Should(Equal("999"))
		Expect(formatInt(12431)).Should(Equal("12,431"))
		Expect(formatInt(1000000)).Should(Equal("1,000,000"))
		Expect(formatInt(-12431)).Should(Equal("-12,431"))
	})

	It("formats unsigned counters with thousands separators", func() {
		Expect(formatCount(0)).Should(Equal("0"))
		Expect(formatCount(12431)).Should(Equal("12,431"))
		Expect(formatCount(1000000)).Should(Equal("1,000,000"))
		Expect(formatCount(^uint64(0))).Should(Equal("18,446,744,073,709,551,615"))
	})

	It("formats percentages and guards against zero totals", func() {
		Expect(formatPercent(41, 100)).Should(Equal("41.0%"))
		Expect(formatPercent(0, 0)).Should(Equal("0.0%"))
	})
})

var _ = Describe("stats command", func() {
	var (
		ts      *httptest.Server
		handler func(w http.ResponseWriter, _ *http.Request)
	)

	JustBeforeEach(func() {
		ts = testHTTPAPIServer(handler)
	})
	JustAfterEach(func() {
		ts.Close()
	})

	When("stats are available", func() {
		BeforeEach(func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				switch {
				case strings.HasPrefix(r.URL.Path, "/api/stats/summary"):
					writeTestJSON(w, sampleSummary())
				case r.URL.Path == "/api/stats/top_domains" && r.URL.Query().Get("blocked") == "true":
					writeTestJSON(w, []nameCount{{Name: "ads.example.com", Count: 402}})
				case r.URL.Path == "/api/stats/top_domains":
					writeTestJSON(w, []nameCount{{Name: "github.com", Count: 1203}})
				case r.URL.Path == "/api/stats/top_clients":
					writeTestJSON(w, []clientCount{{Name: "10.0.0.5", Count: 4310, Blocked: 1204}})
				case r.URL.Path == "/api/stats/query_types":
					writeTestJSON(w, []queryTypeCount{{Type: "A", Count: 9000, Percentage: 72.4}})
				case r.URL.Path == "/api/stats/lists":
					writeTestJSON(w, statsLists{DenylistGroups: 1, ListedDomains: 142000, AllowlistDomains: 30})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}
		})

		It("renders the dashboard to the command output", func() {
			cmd := withContext(newStatsCommand())
			var buf bytes.Buffer
			cmd.SetOut(&buf)

			Expect(stats(cmd, []string{})).Should(Succeed())
			Expect(buf.String()).Should(ContainSubstring("Queries"))
			Expect(buf.String()).Should(ContainSubstring("github.com"))
			Expect(buf.String()).Should(ContainSubstring("Cache:"))
			Expect(buf.String()).Should(ContainSubstring("142,000"))
		})
	})

	When("the days and limit flags are set", func() {
		var seen string

		BeforeEach(func() {
			handler = func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/stats/top_domains" && r.URL.Query().Get("blocked") == "" {
					seen = r.URL.RawQuery
				}

				w.Header().Set("Content-Type", "application/json")

				switch {
				case strings.HasPrefix(r.URL.Path, "/api/stats/summary"):
					writeTestJSON(w, sampleSummary())
				case r.URL.Path == "/api/stats/top_domains",
					r.URL.Path == "/api/stats/top_clients",
					r.URL.Path == "/api/stats/query_types":
					writeTestJSON(w, []nameCount{})
				case r.URL.Path == "/api/stats/lists":
					w.WriteHeader(http.StatusServiceUnavailable)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}
		})

		It("passes them to the API and tolerates disabled lists", func() {
			cmd := withContext(newStatsCommand())
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			Expect(cmd.Flags().Set("days", "30")).Should(Succeed())
			Expect(cmd.Flags().Set("limit", "5")).Should(Succeed())

			Expect(stats(cmd, []string{})).Should(Succeed())
			Expect(seen).Should(ContainSubstring("days=30"))
			Expect(seen).Should(ContainSubstring("limit=5"))
			// a disabled lists endpoint must not fail the command
			Expect(buf.String()).Should(ContainSubstring("Lists: (none)"))
		})
	})

	When("statistics are disabled (503)", func() {
		BeforeEach(func() {
			handler = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
		})
		It("returns a friendly error", func() {
			err := stats(withContext(newStatsCommand()), []string{})
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("disabled"))
		})
	})

	When("server returns 500", func() {
		BeforeEach(func() {
			handler = func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}
		})
		It("returns an error", func() {
			err := stats(withContext(newStatsCommand()), []string{})
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("500 Internal Server Error"))
		})
	})

	When("the API URL is unreachable", func() {
		It("returns a transport error", func() {
			apiPort = 0
			err := stats(withContext(newStatsCommand()), []string{})
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).Should(ContainSubstring("connection refused"))
		})
	})
})

func writeTestJSON(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	Expect(err).Should(Succeed())
	_, err = w.Write(data)
	Expect(err).Should(Succeed())
}
