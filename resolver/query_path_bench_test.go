package resolver

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/helpertest/benchdata"
	"github.com/mia0x75/denyzer/model"

	"github.com/creasty/defaults"
	"github.com/miekg/dns"
)

const (
	// benchBlockingRegexRules is the number of regex rules in the regex-only list
	// group. It is the same order of magnitude as a real OISD-derived list, and it
	// is what makes the linear regex scan visible in the numbers.
	benchBlockingRegexRules = 1000

	// benchBlockingGroups is the number of list groups a multi-group lookup spans.
	benchBlockingGroups = 4

	// benchBlockingClientIP is the client address the benchmark queries come from.
	benchBlockingClientIP = "192.0.2.10"

	// benchBlockingDomain is the base domain the benchmark's query names are
	// built from. It is a plain domain on purpose: the special-use domains
	// (test., example., invalid., localhost.) are answered by the special-use
	// domain resolver, which sits above the upstream in the chain, so a query
	// for one of them never reaches the upstream and is never cached.
	benchBlockingDomain = "query-path-bench.com"
)

// benchBlockingRegexRulesFor loads the regex rules of the regex-only list group
// from the OISD list the repository ships, so the benchmark measures the shape of
// the rules a real deployment uses.
func benchBlockingRegexRulesFor(tb testing.TB) []string {
	tb.Helper()

	rules, err := benchdata.OISDRegexRules(benchBlockingRegexRules)
	if err != nil {
		tb.Fatal(err)
	}

	return rules
}

// benchBlockingResolver builds a blocking resolver whose denylist holds the given
// groups. Its next resolver is a no-op, so a query that is not blocked costs
// nothing beyond the list lookup the benchmark is after.
func benchBlockingResolver(
	b *testing.B, clientGroups, denylists map[string][]string,
) *BlockingResolver {
	b.Helper()

	cfg := config.Blocking{
		ClientGroupsBlock: clientGroups,
		Denylists:         map[string][]config.BytesSource{},
	}

	if err := defaults.Set(&cfg); err != nil {
		b.Fatal(err)
	}

	// No periodic refresh: the benchmark must not reload the lists while it runs.
	cfg.Loading.RefreshPeriod = config.Duration(-1)

	for group, rules := range denylists {
		cfg.Denylists[group] = []config.BytesSource{config.TextBytesSource(rules...)}
	}

	ctx := b.Context()

	bootstrapCfg, err := config.WithDefaults[config.Config]()
	if err != nil {
		b.Fatal(err)
	}

	bootstrap, err := NewBootstrap(ctx, &bootstrapCfg)
	if err != nil {
		b.Fatal(err)
	}

	r, err := NewBlockingResolver(ctx, cfg, bootstrap)
	if err != nil {
		b.Fatal(err)
	}

	r.Next(NewNoOpResolver())

	return r
}

// benchBlockingResolve measures the blocking resolver's own Resolve for one query.
func benchBlockingResolve(b *testing.B, r *BlockingResolver, request *model.Request) {
	b.Helper()

	b.ReportAllocs()

	for b.Loop() {
		if _, err := r.Resolve(b.Context(), request); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBlockingResolve measures the denylist lookup of the blocking resolver,
// the section of the chain the regex pre-filter of the last phase targets.
//
// The cases cover the shapes a lookup can take: a hit in the exact, the wildcard
// and the regex cache, and a miss in a group that holds one, several, or nothing
// but regexes.
//
//	go test -run=^$ -bench=BenchmarkBlockingResolve -benchmem ./resolver/
func BenchmarkBlockingResolve(b *testing.B) {
	multiGroups := make([]string, 0, benchBlockingGroups)
	for i := range benchBlockingGroups {
		multiGroups = append(multiGroups, fmt.Sprintf("grp-%d", i))
	}

	denylists := map[string][]string{
		"grp-exact":    {"exact-blocked." + benchBlockingDomain},
		"grp-wildcard": {"*.wildcard-blocked." + benchBlockingDomain},
		"grp-regex":    {"/^regex-blocked[0-9]*\\." + regexp.QuoteMeta(benchBlockingDomain) + "$/"},
	}

	for i := range benchBlockingGroups {
		denylists[fmt.Sprintf("grp-%d", i)] = []string{fmt.Sprintf("multi-blocked-%d.%s", i, benchBlockingDomain)}
	}

	b.Run("exact_hit", func(b *testing.B) {
		r := benchBlockingResolver(b, map[string][]string{"default": {"grp-exact"}}, denylists)

		benchBlockingResolve(b, r,
			newRequestWithClient("exact-blocked."+benchBlockingDomain, dns.Type(dns.TypeA), benchBlockingClientIP))
	})

	b.Run("wildcard_hit", func(b *testing.B) {
		r := benchBlockingResolver(b, map[string][]string{"default": {"grp-wildcard"}}, denylists)

		benchBlockingResolve(b, r,
			newRequestWithClient("sub.wildcard-blocked."+benchBlockingDomain, dns.Type(dns.TypeA), benchBlockingClientIP))
	})

	b.Run("regex_hit", func(b *testing.B) {
		r := benchBlockingResolver(b, map[string][]string{"default": {"grp-regex"}}, denylists)

		benchBlockingResolve(b, r,
			newRequestWithClient("regex-blocked42."+benchBlockingDomain, dns.Type(dns.TypeA), benchBlockingClientIP))
	})

	b.Run("miss_one_group", func(b *testing.B) {
		r := benchBlockingResolver(b, map[string][]string{"default": {"grp-exact"}}, denylists)

		benchBlockingResolve(b, r,
			newRequestWithClient("allowed."+benchBlockingDomain, dns.Type(dns.TypeA), benchBlockingClientIP))
	})

	b.Run("miss_multi_group", func(b *testing.B) {
		r := benchBlockingResolver(b, map[string][]string{"default": multiGroups}, denylists)

		benchBlockingResolve(b, r,
			newRequestWithClient("allowed."+benchBlockingDomain, dns.Type(dns.TypeA), benchBlockingClientIP))
	})

	b.Run("regex_only_miss", func(b *testing.B) {
		r := benchBlockingResolver(b, map[string][]string{"default": {"grp-regex"}}, map[string][]string{
			"grp-regex": benchBlockingRegexRulesFor(b),
		})

		benchBlockingResolve(b, r,
			newRequestWithClient("www.some-site."+benchBlockingDomain, dns.Type(dns.TypeA), benchBlockingClientIP))
	})
}
