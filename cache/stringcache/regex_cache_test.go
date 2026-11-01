package stringcache

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestRegexRequiredLiteral(t *testing.T) {
	for _, tc := range []struct{ pattern, want string }{
		{`^(.*\.)?example\.com$`, "example.com"},
		{`^prefix[0-9]+`, "prefix"},
		{`(?i)example`, ""},
		{`(?i:foo)bar`, "bar"},
		{`foo|bar`, ""},
		{`foo?`, "fo"},
		{`(?:foo)?`, ""},
		{`(?:foo)+`, "foo"},
		{`(?:foo){0,2}`, ""},
		{`(?:foo){1,2}`, "foo"},
		{`\Qfoo.bar\E`, "foo.bar"},
		{`é`, "é"},
		{`\x{FFFD}`, ""},
		{``, ""},
		{`[`, ""},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			if got := regexRequiredLiteral(tc.pattern); got != tc.want {
				t.Fatalf("required literal = %q, want %q", got, tc.want)
			}
		})
	}
}

func assertRegexOracle(t *testing.T, patterns, queries []string) {
	t.Helper()
	factory := newRegexCacheFactory()
	oracle := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		if !factory.addEntry("/" + pattern + "/") {
			t.Fatalf("pattern not accepted: %q", pattern)
		}
		compiled, err := regexp.Compile(strings.TrimSpace(pattern))
		if err != nil {
			t.Fatal(err)
		}
		oracle = append(oracle, compiled)
	}
	cache := factory.create()
	if cache == nil || cache.elementCount() != len(patterns) {
		t.Fatal("missing cache or incorrect count")
	}
	for _, query := range queries {
		wantRule, wantOK := "", false
		for _, re := range oracle {
			if re.MatchString(query) {
				wantRule, wantOK = "/"+re.String()+"/", true

				break
			}
		}
		gotRule, gotOK := cache.findMatch(query)
		if gotRule != wantRule || gotOK != wantOK {
			t.Fatalf("patterns %q query %q: got (%q, %v), want (%q, %v)",
				patterns, query, gotRule, gotOK, wantRule, wantOK)
		}
	}
}

func TestRegexCacheDifferential(t *testing.T) {
	patterns := []string{
		`^(.*\.)?example\.com$`, `^foo`, `foo$`, `\Afoo\z`, `(?m)^foo$`,
		`foo|bar`, `(?:foo|foobar)baz`, `(?:foo)?bar`, `foo*`, `foo+`,
		`foo{0,3}`, `(?:foo){1,3}`, `(?i)kelvin`, `(?i:foo)Bar`,
		`(?s).*foo.*`, `\Qfoo.bar\E`, `\x66oo`, `\141bc`, `foo\\bar`,
		`\bfoo\b`, `[a-z]+\.test`, `\p{Greek}+`, `bücher\.example`,
		`(?i)k`, `(?i)s`, `�`, `a�b`, `(?P<name>foo)bar`,
		`foo.*longer`, `foo(?:bar|baz)quux`, `a\x00b`, ``, `^$`,
	}
	queries := []string{
		"", ".", "foo", "foobar", "foobaz", "bar", "fo", "fooo", "foo.bar",
		"foo\\bar", "abc", "foo\nbar", "bar\nfoo", "foo\n", "longerfoo",
		"fooXXlonger", "fooBar", "FOOBar", "kelvin", "KELVIN", "ſ", "K",
		"bücher.example", "BÜCHER.example", "αβ", "a\x00b", "�", "\xff", "a\xffb",
		"example.com", "www.example.com", "example.com.invalid", "not-example.com",
	}
	for i, pattern := range patterns {
		t.Run(fmt.Sprintf("pattern_%d", i), func(t *testing.T) {
			assertRegexOracle(t, []string{pattern}, queries)
		})
	}
	// Mixed classified/fallback rules must preserve the first matching rule,
	// not just boolean membership, regardless of insertion order.
	for range 2 {
		assertRegexOracle(t, patterns, queries)
		slices.Reverse(patterns)
	}
}

func TestRegexCacheRealListDifferential(t *testing.T) {
	// regexTestData is derived from the checked-in real OISD wildcard fixture.
	factory := newRegexCacheFactory()
	for _, entry := range regexTestData {
		factory.addEntry(entry)
	}
	cache := factory.create().(regexCache)
	if len(cache) != len(regexTestData) {
		t.Fatalf("compiled %d rules, want %d", len(cache), len(regexTestData))
	}

	// Check every real rule's filter decision against regexp.MatchString for
	// hit, near-hit and malformed-UTF-8 variants. Testing the entries directly
	// avoids an earlier broad rule hiding a false negative in a whole-cache run.
	for i := range regexTestData {
		base := strings.TrimPrefix(wildcardTestData[i], "*.")
		queries := append(queriesAround(base), "\xff"+base, base+"\xff")
		for _, query := range queries {
			entry := cache[i]
			if entry.required != "" && !strings.Contains(query, entry.required) && entry.re.MatchString(query) {
				t.Fatalf("rule %d (%q) filtered a matching query %q", i, entry.re.String(), query)
			}
		}
	}
}

func TestRegexCacheMissAllocations(t *testing.T) {
	factory := newRegexCacheFactory()
	for _, entry := range []string{`/^(.*\.)?example\.com$/`, `/^foo/`, `/(?i)bar/`} {
		factory.addEntry(entry)
	}
	cache := factory.create()
	for _, query := range []string{"unlisted.invalid", "", "\xff"} {
		if _, ok := cache.findMatch(query); ok {
			t.Fatalf("unexpected match: %q", query)
		}
		if got := testing.AllocsPerRun(100, func() { cache.findMatch(query) }); got != 0 {
			t.Fatalf("query %q: %v allocations, want zero", query, got)
		}
	}
}

func FuzzRegexCacheMatchesStandardLibrary(f *testing.F) {
	for _, seed := range [][3]string{
		{`^(.*\.)?example\.com$`, `(?i)foo`, "www.example.com"},
		{`foo|bar`, `(?:foo)?`, "bar"},
		{`a�b`, `(?i)k`, "a\xffb"},
		{`(?:foo){0,3}`, `\Qfoo.bar\E`, "foo.bar"},
		{``, `^$`, ""},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, a, b, query string) {
		for _, pattern := range []string{a, b} {
			if _, err := regexp.Compile(strings.TrimSpace(pattern)); err != nil {
				return
			}
		}
		assertRegexOracle(t, []string{a, b}, []string{query})
	})
}
