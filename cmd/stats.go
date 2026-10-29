package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/spf13/cobra"
)

const (
	statsTimeFormat    = "2006-01-02 15:04"
	statsTypeColLabel  = "Type"
	statsCountColLabel = "Count"
	emptyNote          = "(none)"
	percentMultiplier  = 100
	statsDefaultDays   = 7
	statsDefaultLimit  = 10

	// statsColSecond/statsColThird are 1-based table column indexes used for
	// right-alignment.
	statsColSecond = 2
	statsColThird  = 3
)

// newStatsCommand creates new command instance
func newStatsCommand() *cobra.Command {
	c := &cobra.Command{
		Use:               "stats",
		Args:              cobra.NoArgs,
		Short:             "shows DNS statistics",
		Long:              "Shows a summary of DNS query statistics collected by the running denyzer instance.",
		RunE:              stats,
		PersistentPreRunE: initConfigPreRun,
	}

	c.Flags().IntP("days", "d", statsDefaultDays, "time window in days for top lists and query types")
	c.Flags().IntP("limit", "l", statsDefaultLimit, "number of entries shown in the top lists")

	return c
}

// statsSnapshot aggregates every value rendered by the stats command.
type statsSnapshot struct {
	summary   *statsSummary
	topDomain []nameCount
	topBlkDom []nameCount
	topClient []clientCount
	queryType []queryTypeCount
	lists     *statsLists
	days      int
}

type statsSummary struct {
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

type nameCount struct {
	Name  string `json:"name"`
	Count uint32 `json:"count"`
}

type clientCount struct {
	Name    string `json:"name"`
	Count   uint32 `json:"count"`
	Blocked uint32 `json:"blocked"`
}

type queryTypeCount struct {
	Type       string  `json:"type"`
	Count      uint64  `json:"count"`
	Percentage float64 `json:"percentage"`
}

type statsLists struct {
	DenylistGroups   int `json:"denylistGroups"`
	ListedDomains    int `json:"listedDomains"`
	AllowlistDomains int `json:"allowlistDomains"`
}

func stats(cmd *cobra.Command, _ []string) error {
	days, err := cmd.Flags().GetInt("days")
	if err != nil {
		return err
	}

	limit, err := cmd.Flags().GetInt("limit")
	if err != nil {
		return err
	}

	snapshot, err := fetchStats(cmd.Context(), days, limit)
	if err != nil {
		return err
	}

	renderStats(cmd.OutOrStdout(), snapshot)

	return nil
}

// fetchStats reads every section of the snapshot from the denyzer HTTP API.
func fetchStats(ctx context.Context, days, limit int) (*statsSnapshot, error) {
	summary := new(statsSummary)
	if err := getJSON(ctx, "/api/stats/summary", nil, summary); err != nil {
		return nil, fmt.Errorf("can't read stats summary: %w", err)
	}

	window := url.Values{}
	window.Set("days", strconv.Itoa(days))
	window.Set("limit", strconv.Itoa(limit))

	topDomains := []nameCount{}
	if err := getJSON(ctx, "/api/stats/top_domains", window, &topDomains); err != nil {
		return nil, fmt.Errorf("can't read top domains: %w", err)
	}

	blockedWindow := url.Values{}
	blockedWindow.Set("days", strconv.Itoa(days))
	blockedWindow.Set("limit", strconv.Itoa(limit))
	blockedWindow.Set("blocked", "true")

	topBlocked := []nameCount{}
	if err := getJSON(ctx, "/api/stats/top_domains", blockedWindow, &topBlocked); err != nil {
		return nil, fmt.Errorf("can't read top blocked domains: %w", err)
	}

	topClients := []clientCount{}
	if err := getJSON(ctx, "/api/stats/top_clients", window, &topClients); err != nil {
		return nil, fmt.Errorf("can't read top clients: %w", err)
	}

	queryTypes := []queryTypeCount{}

	typesWindow := url.Values{}
	typesWindow.Set("days", strconv.Itoa(days))

	if err := getJSON(ctx, "/api/stats/query_types", typesWindow, &queryTypes); err != nil {
		return nil, fmt.Errorf("can't read query types: %w", err)
	}

	// Lists are optional: the endpoint returns 503 when no blocklist provider is
	// wired into the resolver chain, which should not fail the whole command.
	lists := new(statsLists)

	if err := getJSON(ctx, "/api/stats/lists", nil, lists); err != nil {
		if errors.Is(err, errStatsUnavailable) {
			lists = nil
		} else {
			return nil, fmt.Errorf("can't read list stats: %w", err)
		}
	}

	return &statsSnapshot{
		summary:   summary,
		topDomain: topDomains,
		topBlkDom: topBlocked,
		topClient: topClients,
		queryType: queryTypes,
		lists:     lists,
		days:      days,
	}, nil
}

// errStatsUnavailable signals that an optional stats endpoint is disabled.
var errStatsUnavailable = errors.New("statistics are disabled")

// getJSON performs a GET request against the denyzer API and decodes the JSON
// response into out. query may be nil when no parameters are needed.
func getJSON(ctx context.Context, path string, query url.Values, out any) error {
	reqURL := apiURL() + strings.TrimPrefix(path, "/api")
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("can't create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("can't execute %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return errStatsUnavailable
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return fmt.Errorf("response NOK, %s %s", resp.Status, strings.TrimSpace(string(body)))
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("can't decode response: %w", err)
	}

	return nil
}

// renderStats writes a human-readable dashboard of the stats snapshot to w.
func renderStats(w io.Writer, s *statsSnapshot) {
	end := time.Now().UTC()
	start := end.AddDate(0, 0, -s.days)

	fmt.Fprintf(w, "Window: %s .. %s (UTC) - top lists cover the last %d day(s)\n\n",
		start.Format(statsTimeFormat), end.Format(statsTimeFormat), s.days)

	renderSummary(w, s.summary)
	renderTopTable(w, "Top Domains", s.topDomain)
	renderTopTable(w, "Top Blocked", s.topBlkDom)
	renderClientTable(w, "Top Clients", s.topClient)
	renderQueryTypes(w, s.queryType)
	renderFooter(w, s)
}

func newStatsTable(w io.Writer, rightAlignCols ...int) table.Writer {
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleLight)

	cfgs := make([]table.ColumnConfig, 0, len(rightAlignCols))
	for _, c := range rightAlignCols {
		cfgs = append(cfgs, table.ColumnConfig{Number: c, Align: text.AlignRight})
	}

	t.SetColumnConfigs(cfgs)

	return t
}

func renderSummary(w io.Writer, s *statsSummary) {
	fmt.Fprintf(w, "Summary (all time)\n")

	t := newStatsTable(w, statsColSecond)
	t.AppendHeader(table.Row{"Metric", "Value"})
	t.AppendRow(table.Row{"Queries", formatCount(s.TotalQueries)})
	t.AppendRow(table.Row{"Cached", withPercent(s.CachedCount, s.TotalQueries)})
	t.AppendRow(table.Row{"Forwarded", formatCount(s.ForwardedCount)})
	t.AppendRow(table.Row{"Blocked", withPercent(s.BlockedCount, s.TotalQueries)})
	t.AppendRow(table.Row{"Filtered", withPercent(s.FilteredCount, s.TotalQueries)})
	t.AppendRow(table.Row{"Local", formatCount(s.LocalCount)})
	t.AppendRow(table.Row{"Dropped", formatCount(s.DroppedCount)})
	t.AppendRow(table.Row{"Errors", formatCount(s.ErrorCount)})
	t.AppendRow(table.Row{"Avg response", fmt.Sprintf("%d ms", s.AvgResponseMs)})
	t.AppendRow(table.Row{"Cache hit rate", fmt.Sprintf("%.1f%%", s.CacheHitRate*percentMultiplier)})
	t.Render()
	fmt.Fprintln(w)
}

func renderTopTable(w io.Writer, title string, rows []nameCount) {
	if len(rows) == 0 {
		fmt.Fprintf(w, "%s: %s\n\n", title, emptyNote)

		return
	}

	fmt.Fprintf(w, "%s\n", title)

	t := newStatsTable(w, statsColSecond)
	t.AppendHeader(table.Row{"Domain", statsCountColLabel})

	for _, r := range rows {
		t.AppendRow(table.Row{r.Name, formatCount(uint64(r.Count))})
	}

	t.Render()
	fmt.Fprintln(w)
}

func renderClientTable(w io.Writer, title string, rows []clientCount) {
	if len(rows) == 0 {
		fmt.Fprintf(w, "%s: %s\n\n", title, emptyNote)

		return
	}

	fmt.Fprintf(w, "%s\n", title)

	t := newStatsTable(w, statsColSecond, statsColThird)
	t.AppendHeader(table.Row{"Client", statsCountColLabel, "Blocked"})

	for _, r := range rows {
		t.AppendRow(table.Row{r.Name, formatCount(uint64(r.Count)), formatCount(uint64(r.Blocked))})
	}

	t.Render()
	fmt.Fprintln(w)
}

func renderQueryTypes(w io.Writer, rows []queryTypeCount) {
	if len(rows) == 0 {
		fmt.Fprintf(w, "By Query Type: %s\n\n", emptyNote)

		return
	}

	items := make([]queryTypeCount, len(rows))
	copy(items, rows)

	sort.Slice(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}

		return items[i].Type < items[j].Type
	})

	fmt.Fprintf(w, "By Query Type\n")

	t := newStatsTable(w, statsColSecond, statsColThird)
	t.AppendHeader(table.Row{statsTypeColLabel, statsCountColLabel, "Share"})

	for _, it := range items {
		t.AppendRow(table.Row{it.Type, formatCount(it.Count), fmt.Sprintf("%.1f%%", it.Percentage)})
	}

	t.Render()
	fmt.Fprintln(w)
}

func renderFooter(w io.Writer, s *statsSnapshot) {
	fmt.Fprintf(w, "Cache: %s entries\n", formatCount(s.summary.CacheEntries))

	if s.lists == nil {
		fmt.Fprintf(w, "Lists: %s\n", emptyNote)

		return
	}

	fmt.Fprintf(w, "Lists\n")

	t := newStatsTable(w, statsColSecond, statsColThird)
	t.AppendHeader(table.Row{"Group Metrics", "Denylist", "Allowlist"})
	t.AppendRow(table.Row{"Groups", formatInt(s.lists.DenylistGroups), "—"})
	t.AppendRow(table.Row{"Domains", formatInt(s.lists.ListedDomains), formatInt(s.lists.AllowlistDomains)})
	t.Render()
	fmt.Fprintln(w)
}

func withPercent(part, total uint64) string {
	return fmt.Sprintf("%s (%s)", formatCount(part), formatPercent(part, total))
}

func formatPercent(part, total uint64) string {
	if total == 0 {
		return "0.0%"
	}

	return fmt.Sprintf("%.1f%%", float64(part)/float64(total)*percentMultiplier)
}

// formatInt renders a signed integer with thousands separators.
func formatInt(n int) string {
	return formatInt64(int64(n))
}

// formatCount renders a non-negative counter with thousands separators.
func formatCount(n uint64) string {
	digits := strconv.FormatUint(n, 10)

	return groupDigits(digits)
}

func formatInt64(n int64) string {
	digits := strconv.FormatInt(n, 10)

	neg := strings.HasPrefix(digits, "-")
	if neg {
		digits = digits[1:]
	}

	grouped := groupDigits(digits)
	if neg {
		return "-" + grouped
	}

	return grouped
}

// groupDigits inserts a comma every three digits of s, which must contain only
// decimal digits.
func groupDigits(s string) string {
	var b strings.Builder

	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}

		b.WriteByte(s[i])
	}

	return b.String()
}
