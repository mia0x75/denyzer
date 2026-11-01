# Regex engine benchmark

## Conclusion

The benchmark measured Denyzer's real regex path, not an isolated
`MatchString` loop:

```text
ChainedGroupedCache.Contains
  -> InMemoryGroupedCache.Contains
  -> regexCache.findMatch
  -> compiled regexp.MatchString
```

The benchmark-backed decision is:

1. **Do not replace the standard library with go-re2.** Its default WASM
   implementation was usually slower on Denyzer's short domain strings and
   substantially slower for large OISD-shaped linear scans. Its `re2_cgo`
   mode requires a system-installed RE2 library through `pkg-config`, which
   does not fit the current Zig/musl cross-build setup.
2. **Do not make coregex a blanket replacement yet.** It was dramatically
   faster for the OISD shape `^(.*\.)?domain$`, but slower than the standard
   library for the literal-prefix shape after the first match. It also has
   materially higher compile cost and retained memory, and is pre-1.0.
3. **Keep regex work in Phase 4 (blocking regex pre-filter).** First avoid
   entering the regex cache when exact or wildcard caches can decide the query.
   Then remeasure the remaining regex-only path. If an engine replacement is
   still justified, evaluate coregex only for the measured pattern class.

This report does not change the production regex implementation.

## Environment and method

- Go `go1.27.3-X:nodwarf5`
- Linux amd64, 16 logical threads
- CPU governor: `schedutil`
- Host otherwise idle during the formal runs
- Fresh repository copy, including the existing `go fix ./...` changes
- Engines:
  - standard library `regexp`
  - `github.com/wasilibs/go-re2` `v1.13.0`
  - `github.com/coregx/coregex` `v0.12.30`
- Three rounds with the engine order rotated between rounds; values below are
  medians of the three observations.
- The scratch selector used build tags only. No interface dispatch was added
  to the measured query path.

The core command was:

```bash
env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy \
  -u SOCKS5_PROXY \
  go test -tags "$ENGINE_TAGS" ./cache/stringcache -run '^$' \
  -bench '^BenchmarkEngine(Chain|Compile)$' -benchmem \
  -benchtime=200ms -count=1
```

The tested regex counts were 1, 10, 50, 100, 500, and 1000. Query cases were
first, middle, last, miss, near-miss, subdomain, exact-cache, and
wildcard-cache. The two regex shapes were:

```text
OISD:   ^(.*\.)?domain$
Prefix: ^regexN\..*$
```

## Query benchmark

Values are `ns/op`; all corresponding regex-hit cases used 3 allocs/op, and
regex misses used 0 B/op and 0 allocs/op.

### OISD-shaped rules

| Regexes | Case | stdlib | coregex | go-re2 |
| ---: | --- | ---: | ---: | ---: |
| 10 | miss | 9,983 | **398** | 9,116 |
| 100 | miss | 72,491 | **1,917** | 79,403 |
| 500 | miss | 460,866 | **7,820** | 450,834 |
| 1000 | miss | 713,841 | **22,780** | 913,159 |
| 10 | middle hit | 4,070 | **1,854** | 6,375 |
| 100 | middle hit | 39,017 | **2,705** | 42,594 |
| 500 | middle hit | 257,355 | **6,004** | 261,451 |
| 1000 | middle hit | 208,218 | **9,832** | 626,203 |
| 10 | last hit | 4,081 | **1,901** | 5,686 |
| 100 | last hit | 22,924 | **2,478** | 71,546 |
| 500 | last hit | 317,065 | **10,783** | 331,304 |
| 1000 | last hit | 44,051 | **17,459** | 466,521 |

First-hit cost was close because the scan stops immediately:

| Regexes | stdlib | coregex | go-re2 |
| ---: | ---: | ---: | ---: |
| 10 | 1,833 ns/op | 1,851 ns/op | 1,953 ns/op |
| 100 | 2,202 ns/op | **1,859 ns/op** | 2,046 ns/op |
| 500 | 2,542 ns/op | 1,898 ns/op | 1,903 ns/op |
| 1000 | 1,856 ns/op | 1,930 ns/op | **1,776 ns/op** |

### Literal-prefix rules

| Regexes | Case | stdlib | coregex | go-re2 |
| ---: | --- | ---: | ---: | ---: |
| 10 | miss | 1,209 | **393** | 4,179 |
| 100 | miss | 8,282 | **1,219** | 18,855 |
| 500 | miss | 38,441 | **6,948** | 91,532 |
| 1000 | miss | 66,024 | **22,312** | 189,877 |
| 10 | middle hit | **1,671** | 3,781 | 2,464 |
| 100 | middle hit | **4,711** | 26,781 | 10,691 |
| 500 | middle hit | **26,261** | 168,450 | 47,967 |
| 1000 | middle hit | **38,572** | 353,263 | 111,768 |
| 10 | last hit | **2,042** | 4,147 | 6,297 |
| 100 | last hit | **9,714** | 33,507 | 22,582 |
| 500 | last hit | **42,807** | 168,309 | 103,212 |
| 1000 | last hit | **74,992** | 472,348 | 177,642 |

Exact and wildcard hits use the existing reverse chain order, so they do not
meaningfully distinguish the regex engines. This is why the Phase 4 pre-filter
has higher priority than a global engine replacement.

## Existing repository benchmark

The repository's own `BenchmarkGroupedCache` was rerun with the same cache
composition and 200 regexes. Median of three runs:

| Benchmark | stdlib | coregex | go-re2 |
| --- | ---: | ---: | ---: |
| `BenchmarkGroupedCacheStringHit` | 730.5 ns/op | **578.1 ns/op** | 548.9 ns/op |
| `BenchmarkGroupedCacheMiss` | 29,725 ns/op | **4,950 ns/op** | 64,969 ns/op |

Exact hits reported `336 B/op, 2 allocs/op` for all engines. The standard
library miss path reported `1 B/op, 0 allocs/op`; the two alternatives
reported `0 B/op, 0 allocs/op`.

## Compile cost and memory

Values are medians of three rounds. Format is `ns/op / B/op / allocs/op` for
building a fresh regex cache.

### OISD-shaped rules

| Regexes | stdlib | coregex | go-re2 |
| ---: | --- | --- | --- |
| 100 | 1.22 ms / 0.83 MB / 4,586 | 11.45 ms / 8.29 MB / 20,054 | 2.97 ms / 0.014 MB / 114 |
| 500 | 6.80 ms / 4.36 MB / 23,026 | 59.03 ms / 42.75 MB / 101,173 | 16.34 ms / 0.067 MB / 538 |
| 1000 | 13.79 ms / 8.63 MB / 46,011 | 132.23 ms / 84.83 MB / 201,704 | 30.34 ms / 0.132 MB / 1,062 |

### Literal-prefix rules

| Regexes | stdlib | coregex | go-re2 |
| ---: | --- | --- | --- |
| 100 | 0.85 ms / 0.44 MB / 5,490 | 8.61 ms / 3.09 MB / 13,990 | 1.96 ms / 0.014 MB / 113 |
| 500 | 3.78 ms / 2.26 MB / 29,092 | 27.19 ms / 15.49 MB / 70,392 | 10.40 ms / 0.066 MB / 528 |
| 1000 | 7.96 ms / 4.54 MB / 58,593 | 59.80 ms / 30.98 MB / 140,893 | 21.33 ms / 0.130 MB / 1,013 |

An independent-process probe confirmed the retained-memory direction at 1000
OISD rules: approximately 8.5 MB RSS for stdlib, 33.5 MB for coregex, and
6.0–6.4 MB for go-re2. The go-re2 number does not mean it is free: its default
implementation is a Go-translated WebAssembly RE2 runtime and its binary has
additional fixed cost.

## Binary, initialization, and build checks

Unstripped `go build .` sizes in the scratch copy were:

| Engine | Binary size | Change vs stdlib |
| --- | ---: | ---: |
| stdlib | 37,321,584 bytes | — |
| coregex | 38,536,808 bytes | +3.3% |
| go-re2 | 42,883,408 bytes | +14.9% |

Direct `GODEBUG=inittrace=1` runs showed no material explicit initializer
cost. coregex adds `simd`, `prefilter`, and `meta` package init entries; the
measured entries together were about 0.01 ms in the probe. The important
go-re2 cost is its WASM runtime and binary/RSS footprint rather than a large
named Go initializer.

Representative `CGO_ENABLED=0 go test -c` checks passed for all three engines
on `linux/amd64`, `linux/arm64`, and `linux/mipsle`. This confirms that the
coregex non-amd64 fallback is buildable for representative targets; it is not
a performance claim for those targets.

The go-re2 `re2_cgo` build was tested and failed because this host has no
`re2.pc`/system RE2 installation. The package documentation requires that
system dependency, which would complicate the existing Zig cross-build and
musl release path.

## Correctness and API checks

- Differential testing compared standard-library and selected-engine results
  over 2,007 patterns and 6,010 query strings: 12,062,070 match checks.
- It covered OISD and prefix rules, configured examples, empty and Unicode
  inputs, malformed patterns, `String`, `MatchString`, and
  `ReplaceAllString`.
- All three variants passed the existing `cache/stringcache` test suite.
- The full repository compiled with `go test ./... -run '^$'` for both
  alternatives in the scratch copy.
- coregex documents the required drop-in methods and requires Go 1.27+, which
  is satisfied by the repository's current Go version. It remains pre-1.0, so
  API stability is an additional maintenance cost.

Other production regex call sites (`resolver/caching_resolver.go`,
`resolver/query_logging_resolver.go`, `util/common.go`, `config/upstream.go`,
and `lists/parsers/hosts.go`) were not changed by this benchmark. A future
engine change must validate those call sites separately, especially the shared
`*regexp.Regexp` types in the caching resolver.

## Reproduction notes

Raw benchmark outputs and the temporary harness were kept under
`/tmp/opencode` during the experiment. Reproduce on an otherwise idle host,
keep the governor at `schedutil`, rotate engine order between rounds, and do
not run another compile or benchmark concurrently.

## Appendix: benchmark source code

The following is the complete core of the temporary scratch harness. It was
kept outside the repository's production packages under
`/tmp/opencode/denyzer-bench`.

### Build-time engine selector

The selector used mutually exclusive build tags. The default build selected
the standard library; `-tags bench_coregex` selected coregex and
`-tags bench_re2` selected go-re2.

```go
// internal/benchregexp/stdlib.go
//go:build !bench_re2 && !bench_coregex

package benchregexp

import "regexp"

type Regexp = regexp.Regexp

var Compile = regexp.Compile
var MustCompile = regexp.MustCompile

const Engine = "stdlib"
```

```go
// internal/benchregexp/coregex.go
//go:build bench_coregex && !bench_re2

package benchregexp

import "github.com/coregx/coregex"

type Regexp = coregex.Regexp

var Compile = coregex.Compile
var MustCompile = coregex.MustCompile

const Engine = "coregex"
```

```go
// internal/benchregexp/re2.go
//go:build bench_re2 && !bench_coregex

package benchregexp

import re2 "github.com/wasilibs/go-re2"

type Regexp = re2.Regexp

var Compile = re2.Compile
var MustCompile = re2.MustCompile

const Engine = "go-re2"
```

### Real cache-path benchmark

This test lives in `cache/stringcache`, so it can use the package's actual
`ChainedGroupedCache`, `InMemoryGroupedCache`, regex factory, and OISD test data.

```go
package stringcache

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"testing"

	engine "github.com/mia0x75/denyzer/internal/benchregexp"
)

// Patterns use the repository's actual OISD-derived data, in file order.
// Prefix patterns match the shape in grouped_cache_lookup_bench_test.go.
func enginePatterns(shape string, n int) ([]string, []string) {
	patterns, domains := make([]string, n), make([]string, n)
	for i := range n {
		if shape == "oisd" {
			patterns[i] = regexTestData[i]
			domains[i] = strings.TrimPrefix(wildcardTestData[i], "*.")
		} else {
			patterns[i] = fmt.Sprintf(`/^regex%d\..*$/`, i)
			domains[i] = fmt.Sprintf("regex%d.example.com", i)
		}
	}
	return patterns, domains
}

func engineChain(tb testing.TB, patterns []string) *ChainedGroupedCache {
	tb.Helper()
	c := NewChainedGroupedCache(
		NewInMemoryGroupedRegexCache(),
		NewInMemoryGroupedWildcardCache(),
		NewInMemoryGroupedStringCache(),
	)
	f := c.Refresh("default")
	for _, p := range patterns {
		if !f.AddEntry(p) {
			tb.Fatalf("entry rejected: %s", p)
		}
	}
	for i := range 200 {
		f.AddEntry(fmt.Sprintf("*.wild%d.example", i))
	}
	for i := range 5000 {
		f.AddEntry(fmt.Sprintf("string%d.example.com", i))
	}
	f.Finish()
	if got := c.ElementCount("default"); got != len(patterns)+5200 {
		tb.Fatalf("count %d", got)
	}
	return c
}

func BenchmarkEngineChain(b *testing.B) {
	for _, shape := range []string{"oisd", "prefix"} {
		for _, n := range []int{1, 10, 50, 100, 500, 1000} {
			patterns, domains := enginePatterns(shape, n)
			c := engineChain(b, patterns)
			cases := []struct{ name, domain string }{
				{"first", domains[0]},
				{"middle", domains[n/2]},
				{"last", domains[n-1]},
				{"subdomain", "a.b.c." + domains[n-1]},
				{"miss", "not-present.example.org"},
				{"near-miss", domains[n-1] + ".invalid"},
				{"short-miss", "x.io"},
				{"exact", "string1234.example.com"},
				{"wildcard", "a.wild123.example"},
			}

			// Calculate the expected rule independently with the standard library.
			refs := make([]*regexp.Regexp, len(patterns))
			for i, p := range patterns {
				refs[i] = regexp.MustCompile(p[1 : len(p)-1])
			}
			for _, tc := range cases {
				want := ""
				for _, r := range refs {
					if r.MatchString(tc.domain) {
						want = "/" + r.String() + "/"
						break
					}
				}
				if tc.name == "exact" {
					want = tc.domain
				}
				if tc.name == "wildcard" {
					want = "*.wild123.example"
				}

				b.Run(fmt.Sprintf("%s/n=%d/%s", shape, n, tc.name), func(b *testing.B) {
					groups := []string{"default"}
					if got := c.Contains(tc.domain, groups)["default"]; got != want {
						b.Fatalf("got %q want %q", got, want)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						_ = c.Contains(tc.domain, groups)
					}
				})
			}
		}
	}
}

func BenchmarkEngineCompile(b *testing.B) {
	for _, shape := range []string{"oisd", "prefix"} {
		for _, n := range []int{1, 10, 50, 100, 500, 1000} {
			patterns, _ := enginePatterns(shape, n)
			b.Run(fmt.Sprintf("%s/n=%d", shape, n), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					f := newRegexCacheFactory()
					for _, p := range patterns {
						if !f.addEntry(p) {
							b.Fatal(p)
						}
					}
					if f.count() != n {
						b.Fatalf("compiled %d want %d", f.count(), n)
					}
					_ = f.create()
				}
			})
		}
	}
}
```

### Differential correctness and retained-memory checks

The correctness test compared every selected engine against the standard
library. It also checked replacement behavior because `util.Obfuscate` uses
`ReplaceAllString`.

```go
func TestEngineDifferential(t *testing.T) {
	patterns, domains := enginePatterns("oisd", 1000)
	pp, dd := enginePatterns("prefix", 1000)
	patterns, domains = append(patterns, pp...), append(domains, dd...)
	patterns = append(patterns,
		`/.*google.com/`, `/^apple\.(de|com)$/`, `/amazon/`,
		`/(?i)example\.com$/`, `/^$/`, `/\bads\b/`, `/[a-zA-Z0-9]/`,
	)
	queries := []string{
		"", "x.io", "not-present.example.org", "EXAMPLE.COM", "apple.de",
		"www.google.com", "amazon", "ads.example", "例子.测试", "a\x00b",
	}
	for _, d := range domains {
		queries = append(queries, d, "a.b."+d, d+".invalid")
	}

	checks := 0
	for _, p := range patterns {
		p = p[1 : len(p)-1]
		ref, err := regexp.Compile(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := engine.Compile(p)
		if err != nil {
			t.Fatalf("%s compile %q: %v", engine.Engine, p, err)
		}
		if got.String() != ref.String() {
			t.Fatalf("String mismatch %q", p)
		}
		for _, q := range queries {
			if a, b := got.MatchString(q), ref.MatchString(q); a != b {
				t.Fatalf("%s pattern=%q query=%q got=%v want=%v", engine.Engine, p, q, a, b)
			}
			checks++
		}
	}

	for _, p := range []string{"*", "?", "+", "[", "(wrongRegex", `\q`} {
		_, a := regexp.Compile(p)
		_, b := engine.Compile(p)
		if (a == nil) != (b == nil) {
			t.Fatalf("compile acceptance %q: %v / %v", p, a, b)
		}
	}
	for _, p := range []string{`[a-zA-Z0-9]`, `(example)`, `^|$`} {
		a, b := regexp.MustCompile(p), engine.MustCompile(p)
		for _, q := range queries[:10] {
			for _, replacement := range []string{"*", "$1", "${1}-x"} {
				if a.ReplaceAllString(q, replacement) != b.ReplaceAllString(q, replacement) {
					t.Fatalf("replacement mismatch %q %q", p, q)
				}
			}
		}
	}
	t.Logf("engine=%s patterns=%d queries=%d match_checks=%d", engine.Engine, len(patterns), len(queries), checks)
}

func TestEngineRetainedMemory(t *testing.T) {
	for _, shape := range []string{"oisd", "prefix"} {
		for _, n := range []int{100, 500, 1000} {
			patterns, _ := enginePatterns(shape, n)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			f := newRegexCacheFactory()
			for _, p := range patterns {
				if !f.addEntry(p) {
					t.Fatal(p)
				}
			}
			cache := f.create()
			runtime.ReadMemStats(&after)
			if cache == nil {
				t.Fatal("empty cache")
			}
			t.Logf("engine=%s shape=%s n=%d heap_delta=%d alloc_delta=%d",
				engine.Engine, shape, n,
				after.HeapAlloc-before.HeapAlloc,
				after.TotalAlloc-before.TotalAlloc)
			_ = cache
		}
	}
}
```

### Independent compile/RSS probe

The probe was built three times with the same selector tags. Running it as a
separate process avoided retaining allocations from one engine or one regex
count while measuring another.

```go
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mia0x75/denyzer/internal/benchregexp"
)

func main() {
	n := flag.Int("n", 1000, "number of regexes")
	shape := flag.String("shape", "oisd", "pattern shape: oisd or prefix")
	flag.Parse()

	patterns := make([]string, *n)
	for i := range patterns {
		if *shape == "prefix" {
			patterns[i] = fmt.Sprintf(`^regex%d\..*$`, i)
		} else {
			patterns[i] = fmt.Sprintf(`^(.*\.)?domain%d\.example\.com$`, i)
		}
	}

	start := time.Now()
	compiled := make([]*benchregexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		r, err := benchregexp.Compile(pattern)
		if err != nil {
			panic(err)
		}
		compiled = append(compiled, r)
	}
	runtime.KeepAlive(compiled)

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	matched := false
	if len(compiled) > 0 {
		matched = compiled[len(compiled)-1].MatchString("a.domain999.example.com")
	}
	fmt.Printf(
		"engine=%s shape=%s n=%d compile_ns=%d heap_alloc=%d heap_sys=%d rss_kb=%d match=%v\n",
		benchregexp.Engine, *shape, *n, time.Since(start).Nanoseconds(),
		stats.HeapAlloc, stats.HeapSys, rssKB(), matched,
	)
}

func rssKB() int64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmRSS:" {
			value, _ := strconv.ParseInt(fields[1], 10, 64)
			return value
		}
	}
	return -1
}
```
