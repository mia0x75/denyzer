package stringcache

import (
	"fmt"
	"regexp"
	"testing"
)

// BenchmarkRegexFilter compares the production cache to its original ordered
// regexp-only scan using identical compiled rules and first-match reporting.
func BenchmarkRegexFilter(b *testing.B) {
	for _, shape := range []string{"suffix", "prefix", "fallback"} {
		for _, count := range []int{1, 10, 50, 100, 500, 1000} {
			benchmarkRegexFilterShape(b, shape, count)
		}
	}
}

func benchmarkRegexFilterShape(b *testing.B, shape string, count int) {
	b.Helper()

	patterns := regexBenchPatterns(shape, count)
	factory := newRegexCacheFactory()
	plain := make([]*regexp.Regexp, count)
	for i, pattern := range patterns {
		factory.addEntry("/" + pattern + "/")
		plain[i] = regexp.MustCompile(pattern)
	}
	cache := factory.create()
	queries := []struct{ name, value string }{
		{"first", "rule0000.example.test"},
		{"middle", fmt.Sprintf("rule%04d.example.test", count/2)},
		{"last", fmt.Sprintf("rule%04d.example.test", count-1)},
		{"miss", "unlisted-domain.invalid.test"},
		{"near_miss", fmt.Sprintf("rule%04d.example.test.invalid", count-1)},
		{"short", "x"},
	}
	for _, query := range queries {
		for _, filtered := range []bool{false, true} {
			benchmarkRegexFilterCase(b, shape, count, plain, cache, query, filtered)
		}
	}
}

func benchmarkRegexFilterCase(
	b *testing.B,
	shape string,
	count int,
	plain []*regexp.Regexp,
	cache stringCache,
	query struct{ name, value string },
	filtered bool,
) {
	b.Helper()
	mode := "standard"
	if filtered {
		mode = "filtered"
	}
	b.Run(fmt.Sprintf("%s/rules=%d/%s/%s", shape, count, query.name, mode), func(b *testing.B) {
		lookup := func(s string) (string, bool) { return regexBenchPlain(plain, s) }
		if filtered {
			lookup = cache.findMatch
		}
		wantRule, wantOK := regexBenchPlain(plain, query.value)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			rule, ok := lookup(query.value)
			if rule != wantRule || ok != wantOK {
				b.Fatal("lookup differs from standard-library scan")
			}
		}
	})
}

func regexBenchPatterns(shape string, count int) []string {
	patterns := make([]string, count)
	for i := range patterns {
		switch shape {
		case "suffix":
			patterns[i] = fmt.Sprintf(`^(.*\.)?rule%04d\.example\.test$`, i)
		case "prefix":
			patterns[i] = fmt.Sprintf(`^rule%04d\.example\.test$`, i)
		case "fallback":
			patterns[i] = fmt.Sprintf(`(?i)^rule%04d\.example\.test$`, i)
		}
	}

	return patterns
}

func regexBenchPlain(patterns []*regexp.Regexp, query string) (string, bool) {
	for _, pattern := range patterns {
		if pattern.MatchString(query) {
			logMatch("regex_cache", "regex", pattern.String(), query)

			return "/" + pattern.String() + "/", true
		}
	}

	return "", false
}

func BenchmarkRegexFilterBuild(b *testing.B) {
	for _, shape := range []string{"suffix", "prefix", "fallback"} {
		patterns := regexBenchPatterns(shape, 1000)
		b.Run(shape, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				factory := newRegexCacheFactory()
				for _, pattern := range patterns {
					factory.addEntry("/" + pattern + "/")
				}
				if factory.create().elementCount() != len(patterns) {
					b.Fatal("incorrect count")
				}
			}
		})
	}
}
