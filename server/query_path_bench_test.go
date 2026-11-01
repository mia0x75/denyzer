package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/helpertest/benchdata"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/resolver"
	"github.com/mia0x75/denyzer/util"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

const (
	// benchQueryPathTTL is the TTL of the mock upstream's answer, in seconds. It is
	// long enough for a cached entry to stay valid for the whole benchmark run.
	benchQueryPathTTL = 300

	// benchQueryPathRegexRules is the number of regex rules in the regex-heavy list
	// group. It is the same order of magnitude as a real OISD-derived list, and it
	// is what makes the linear regex scan visible in the numbers.
	benchQueryPathRegexRules = 1000

	// benchQueryPathClientIP is the client address the benchmark queries come from.
	benchQueryPathClientIP = "192.0.2.10"

	// benchQueryPathGroups is the number of list groups a multi-group lookup spans.
	benchQueryPathGroups = 4

	// benchQueryPathDomain is the base domain the benchmark's query names are
	// built from. It is a plain domain on purpose: the special-use domains
	// (test., example., invalid., localhost.) are answered by the special-use
	// domain resolver, which sits above the upstream in the chain, so a query
	// for one of them never reaches the upstream and is never cached.
	benchQueryPathDomain = "query-path-bench.com"
)

// benchQueryPathEnv holds the query chain under test.
type benchQueryPathEnv struct {
	server *Server
}

// benchQueryPathConfig describes the chain a benchmark case needs.
type benchQueryPathConfig struct {
	// cache enables the caching resolver. Without it every query reaches the upstream.
	cache bool
	// clientGroups maps a client identifier to the list groups that apply to it.
	// An empty map disables the blocking resolver, so the case measures the
	// chain without a list lookup.
	clientGroups map[string][]string
	// denylists maps a list group name to the rules it holds.
	denylists map[string][]string
	// emptyAnswer makes the mock upstream reply with NODATA instead of an
	// address record. A response without address records does not trigger the
	// blocking resolver's answer check, so the case measures a single scan of
	// the query name.
	emptyAnswer bool
}

// benchQueryPathRegexRulesFor loads the regex rules of the regex-heavy list group
// from the OISD list the repository ships, so the benchmark measures the shape of
// the rules a real deployment uses.
func benchQueryPathRegexRulesFor(tb testing.TB) []string {
	tb.Helper()

	rules, err := benchdata.OISDRegexRules(benchQueryPathRegexRules)
	if err != nil {
		tb.Fatal(err)
	}

	return rules
}

// newBenchQueryPathEnv builds the query resolver chain and the server that drives
// it. The chain talks to a mock UDP upstream over a real socket, so a query that
// no earlier resolver answers reaches an upstream exactly like in production.
func newBenchQueryPathEnv(b *testing.B, cfg benchQueryPathConfig) *benchQueryPathEnv {
	b.Helper()

	// The mock upstream is created without its constructor because that one
	// registers a Ginkgo cleanup, which is not available in a benchmark.
	mock := &resolver.MockUDPUpstreamServer{}

	if cfg.emptyAnswer {
		mock.WithAnswerFn(func(_ *dns.Msg) *dns.Msg { return new(dns.Msg) })
	} else {
		mock.WithAnswerRR(fmt.Sprintf("answer.%s. %d IN A 192.0.2.1", benchQueryPathDomain, benchQueryPathTTL))
	}

	b.Cleanup(mock.Close)

	c, err := config.WithDefaults[config.Config]()
	if err != nil {
		b.Fatal(err)
	}

	c.Upstreams.Groups = map[string][]config.Upstream{"default": {mock.Start()}}
	c.Upstreams.Timeout = config.Duration(5 * time.Second)
	c.QueryLog.Type = config.QueryLogTypeNone

	if !cfg.cache {
		// A negative max caching time disables the caching resolver, so every
		// query of the benchmark reaches the upstream.
		c.Caching.MaxCachingTime = config.Duration(-1)
	}

	c.Blocking.Loading.RefreshPeriod = config.Duration(-1)
	c.Blocking.ClientGroupsBlock = cfg.clientGroups
	c.Blocking.Denylists = make(map[string][]config.BytesSource, len(cfg.denylists))

	for group, rules := range cfg.denylists {
		c.Blocking.Denylists[group] = []config.BytesSource{config.TextBytesSource(rules...)}
	}

	ctx := context.Background()

	bootstrap, err := resolver.NewBootstrap(ctx, &c)
	if err != nil {
		b.Fatal(err)
	}

	chain, err := createQueryResolver(ctx, &c, bootstrap)
	if err != nil {
		b.Fatal(err)
	}

	return &benchQueryPathEnv{server: &Server{cfg: &c, queryResolver: chain}}
}

// benchQueryPathRequest builds the request the benchmark resolves.
//
// The request is built once per benchmark case and reused: with the default
// configuration no resolver in the chain mutates the query message, so the
// benchmark measures the chain and not the message construction.
func benchQueryPathRequest(domain string) *model.Request {
	return &model.Request{
		ClientIP: net.ParseIP(benchQueryPathClientIP),
		Protocol: model.RequestProtocolUDP,
		Req:      util.NewMsgWithQuestion(domain, dns.Type(dns.TypeA)),
	}
}

// benchQueryPathLogLevel sets the global log level for the duration of the
// benchmark and restores the previous level and output afterwards. The output is
// discarded, but the entries are still formatted, so the measurement includes the
// cost of emitting the log lines.
func benchQueryPathLogLevel(b *testing.B, level logrus.Level) {
	b.Helper()

	logger := log.Log()
	oldLevel := logger.GetLevel()
	oldOut := logger.Out

	logger.SetOutput(io.Discard)
	logger.SetLevel(level)

	b.Cleanup(func() {
		logger.SetLevel(oldLevel)
		logger.SetOutput(oldOut)
	})
}

// benchQueryPathResolve runs the benchmark body for one query case.
func benchQueryPathResolve(b *testing.B, env *benchQueryPathEnv, request *model.Request) {
	b.Helper()

	b.ReportAllocs()

	for b.Loop() {
		ctx := context.Background()

		response, err := env.server.resolve(ctx, request)
		if err != nil {
			b.Fatal(err)
		}

		if response.Res == nil {
			b.Fatal("no response")
		}
	}
}

// benchQueryPathPrimeCache resolves the request once, so its answer is cached,
// and verifies that the second resolution is served from the cache. The
// benchmark that follows then measures the cache hit path.
func benchQueryPathPrimeCache(b *testing.B, env *benchQueryPathEnv, request *model.Request) {
	b.Helper()

	if _, err := env.server.resolve(context.Background(), request); err != nil {
		b.Fatal(err)
	}

	hit, err := env.server.resolve(context.Background(), request)
	if err != nil {
		b.Fatal(err)
	}

	if hit.RType != model.ResponseTypeCACHED {
		b.Fatalf("expected a cached response, got %s", hit.RType)
	}
}

// benchQueryPathExpect resolves the request once and verifies the response
// type and the matched rule. It keeps a benchmark case from silently measuring
// a different path than the one its name claims.
func benchQueryPathExpect(
	b *testing.B, env *benchQueryPathEnv, request *model.Request,
	wantType model.ResponseType, wantRule string,
) {
	b.Helper()

	response, err := env.server.resolve(context.Background(), request)
	if err != nil {
		b.Fatal(err)
	}

	if response.RType != wantType {
		b.Fatalf("expected response type %s, got %s", wantType, response.RType)
	}

	if wantRule != "" && !strings.Contains(response.Reason, wantRule) {
		b.Fatalf("expected reason to contain %q, got %q", wantRule, response.Reason)
	}
}

// BenchmarkQueryPath measures the complete query path: the server's resolve
// entry point, the whole resolver chain, and — for queries no resolver answers
// locally — a real UDP round trip to the mock upstream.
//
// Every case isolates one path, so a change to one of them can be attributed
// to the section it exercises:
//
//   - upstream_miss: the full miss path — one list group, the whole chain and
//     the upstream round trip;
//
//   - upstream_hit: the chain and the cache, without a list lookup;
//
//   - blocked_exact, blocked_wildcard, blocked_regex: the cheap paths that
//     answer the query locally;
//
//   - regex_only_miss: a group that holds nothing but regexes and does not
//     match, so the linear regex scan dominates;
//
//   - one_group and multi_group: the same cache-hit path with one and with
//     four list groups, which exposes the per-group lookup cost.
//
//     go test -run=^$ -bench=BenchmarkQueryPath -benchmem ./server/
func BenchmarkQueryPath(b *testing.B) {
	exactGroup := []string{"grp-exact"}
	wildcardGroup := []string{"grp-wildcard"}
	regexGroup := []string{"grp-regex"}

	multiGroups := make([]string, 0, benchQueryPathGroups)
	for i := range benchQueryPathGroups {
		multiGroups = append(multiGroups, fmt.Sprintf("grp-%d", i))
	}

	denylists := map[string][]string{
		"grp-exact":    {"exact-blocked." + benchQueryPathDomain},
		"grp-wildcard": {"*.wildcard-blocked." + benchQueryPathDomain},
		"grp-regex":    {"/^regex-blocked[0-9]*\\." + regexp.QuoteMeta(benchQueryPathDomain) + "$/"},
	}

	for i := range benchQueryPathGroups {
		denylists[fmt.Sprintf("grp-%d", i)] = []string{fmt.Sprintf("multi-blocked-%d.%s", i, benchQueryPathDomain)}
	}

	b.Run("upstream_miss", func(b *testing.B) {
		env := newBenchQueryPathEnv(b, benchQueryPathConfig{
			clientGroups: map[string][]string{"default": exactGroup},
			denylists:    denylists,
		})

		request := benchQueryPathRequest("allowed." + benchQueryPathDomain)

		benchQueryPathExpect(b, env, request, model.ResponseTypeRESOLVED, "")
		benchQueryPathResolve(b, env, request)
	})

	b.Run("upstream_hit", func(b *testing.B) {
		env := newBenchQueryPathEnv(b, benchQueryPathConfig{
			cache:     true,
			denylists: denylists,
		})

		request := benchQueryPathRequest("cached." + benchQueryPathDomain)

		benchQueryPathExpect(b, env, request, model.ResponseTypeRESOLVED, "")
		benchQueryPathPrimeCache(b, env, request)
		benchQueryPathResolve(b, env, request)
	})

	b.Run("blocked_exact", func(b *testing.B) {
		env := newBenchQueryPathEnv(b, benchQueryPathConfig{
			clientGroups: map[string][]string{"default": exactGroup},
			denylists:    denylists,
		})

		request := benchQueryPathRequest("exact-blocked." + benchQueryPathDomain)

		benchQueryPathExpect(b, env, request, model.ResponseTypeBLOCKED,
			"exact-blocked."+benchQueryPathDomain)
		benchQueryPathResolve(b, env, request)
	})

	b.Run("blocked_wildcard", func(b *testing.B) {
		env := newBenchQueryPathEnv(b, benchQueryPathConfig{
			clientGroups: map[string][]string{"default": wildcardGroup},
			denylists:    denylists,
		})

		request := benchQueryPathRequest("sub.wildcard-blocked." + benchQueryPathDomain)

		benchQueryPathExpect(b, env, request, model.ResponseTypeBLOCKED,
			"*.wildcard-blocked."+benchQueryPathDomain)
		benchQueryPathResolve(b, env, request)
	})

	b.Run("blocked_regex", func(b *testing.B) {
		env := newBenchQueryPathEnv(b, benchQueryPathConfig{
			clientGroups: map[string][]string{"default": regexGroup},
			denylists:    denylists,
		})

		request := benchQueryPathRequest("regex-blocked42." + benchQueryPathDomain)

		benchQueryPathExpect(b, env, request, model.ResponseTypeBLOCKED,
			"/^regex-blocked[0-9]*\\."+regexp.QuoteMeta(benchQueryPathDomain)+"$/")
		benchQueryPathResolve(b, env, request)
	})

	b.Run("regex_only_miss", func(b *testing.B) {
		env := newBenchQueryPathEnv(b, benchQueryPathConfig{
			cache:        true,
			clientGroups: map[string][]string{"default": regexGroup},
			denylists: map[string][]string{
				"grp-regex": benchQueryPathRegexRulesFor(b),
			},
			emptyAnswer: true,
		})

		request := benchQueryPathRequest("not-in-any-list." + benchQueryPathDomain)

		benchQueryPathExpect(b, env, request, model.ResponseTypeRESOLVED, "")
		benchQueryPathPrimeCache(b, env, request)
		benchQueryPathResolve(b, env, request)
	})

	b.Run("regex_late_hit", func(b *testing.B) {
		rules := benchQueryPathRegexRulesFor(b)
		pattern := `/^(.*\.)?late-hit\.` + regexp.QuoteMeta(benchQueryPathDomain) + `$/`
		rules = append(rules, pattern)
		env := newBenchQueryPathEnv(b, benchQueryPathConfig{
			clientGroups: map[string][]string{"default": regexGroup},
			denylists:    map[string][]string{"grp-regex": rules},
		})
		request := benchQueryPathRequest("late-hit." + benchQueryPathDomain)

		benchQueryPathExpect(b, env, request, model.ResponseTypeBLOCKED, pattern)
		benchQueryPathResolve(b, env, request)
	})

	b.Run("one_group", func(b *testing.B) {
		env := newBenchQueryPathEnv(b, benchQueryPathConfig{
			cache:        true,
			clientGroups: map[string][]string{"default": exactGroup},
			denylists:    denylists,
		})

		request := benchQueryPathRequest("allowed." + benchQueryPathDomain)

		benchQueryPathExpect(b, env, request, model.ResponseTypeRESOLVED, "")
		benchQueryPathPrimeCache(b, env, request)
		benchQueryPathResolve(b, env, request)
	})

	b.Run("multi_group", func(b *testing.B) {
		env := newBenchQueryPathEnv(b, benchQueryPathConfig{
			cache:        true,
			clientGroups: map[string][]string{"default": multiGroups},
			denylists:    denylists,
		})

		request := benchQueryPathRequest("allowed." + benchQueryPathDomain)

		benchQueryPathExpect(b, env, request, model.ResponseTypeRESOLVED, "")
		benchQueryPathPrimeCache(b, env, request)
		benchQueryPathResolve(b, env, request)
	})
}

// BenchmarkQueryPathLogLevel measures the same query path with the log level
// turned up. The cost of a log line is only visible when the line is actually
// emitted, so the Debug and Trace cases are what the lazy logging plumbing of
// the next phase has to keep cheap.
//
//	go test -run=^$ -bench=BenchmarkQueryPathLogLevel -benchmem ./server/
func BenchmarkQueryPathLogLevel(b *testing.B) {
	for _, level := range []logrus.Level{logrus.InfoLevel, logrus.DebugLevel, logrus.TraceLevel} {
		b.Run(level.String(), func(b *testing.B) {
			benchQueryPathLogLevel(b, level)

			env := newBenchQueryPathEnv(b, benchQueryPathConfig{
				clientGroups: map[string][]string{"default": {"grp-exact"}},
				denylists: map[string][]string{
					"grp-exact": {"exact-blocked." + benchQueryPathDomain},
				},
			})

			benchQueryPathResolve(b, env, benchQueryPathRequest("exact-blocked."+benchQueryPathDomain))
		})
	}
}

// BenchmarkNewRequest measures the server boundary cost: the request-scoped log
// entry and the model.Request the resolver chain receives.
//
//	go test -run=^$ -bench=BenchmarkNewRequest -benchmem ./server/
func BenchmarkNewRequest(b *testing.B) {
	msg := util.NewMsgWithQuestion("new-request."+benchQueryPathDomain, dns.Type(dns.TypeA))
	clientIP := net.ParseIP(benchQueryPathClientIP)

	b.ReportAllocs()

	for b.Loop() {
		_, request := newRequest(
			context.Background(), clientIP, "client-1", model.RequestProtocolUDP, msg)

		if request == nil {
			b.Fatal("no request")
		}
	}
}
