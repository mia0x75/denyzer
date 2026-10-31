package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/resolver"
)

const (
	// benchListEntries is the number of entries in the synthetic denylist.
	benchListEntries = 10_000

	// benchSourceDelay is the artificial latency of the list source and of the
	// mock upstream that answers the upstream initialization test query. Both
	// are I/O bound, like the real startup work they stand in for.
	benchSourceDelay = 200 * time.Millisecond
)

// benchListServer serves a synthetic denylist after benchSourceDelay.
func benchListServer(tb testing.TB) *httptest.Server {
	tb.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(benchSourceDelay)

		for i := range benchListEntries {
			_, _ = fmt.Fprintf(w, "domain-%d.example.com\n", i)
		}
	}))
	tb.Cleanup(server.Close)

	return server
}

// newQueryResolverPartsSequential is the resolver-at-a-time implementation the
// concurrent one replaced. It is the reference of the benchmark below.
func newQueryResolverPartsSequential(
	ctx context.Context,
	cfg *config.Config,
	bootstrap *resolver.Bootstrap,
) *queryResolverParts {
	parts := &queryResolverParts{
		customDNS: resolver.NewCustomDNSResolver(cfg.CustomDNS),
	}

	parts.upstreamTree, parts.utErr = resolver.NewUpstreamTreeResolver(ctx, cfg.Upstreams, bootstrap)
	parts.blocking, parts.blErr = resolver.NewBlockingResolver(ctx, cfg.Blocking, bootstrap)
	parts.queryLogging, parts.qlErr = resolver.NewQueryLoggingResolver(ctx, cfg.QueryLog)
	parts.condUpstream, parts.cuErr = resolver.NewConditionalUpstreamResolver(
		ctx, cfg.Conditional, cfg.Upstreams, bootstrap)
	parts.hostsFile, parts.hfErr = resolver.NewHostsFileResolver(ctx, cfg.HostsFile, bootstrap)
	parts.cachingResolver, parts.crErr = resolver.NewCachingResolver(ctx, cfg.Caching, cfg.DNSSEC)

	parts.clientNames, parts.cnErr = resolver.NewClientNamesResolver(
		ctx, cfg.ClientLookup, cfg.Upstreams, bootstrap, parts.customDNS, parts.hostsFile)
	parts.dnssecResolver, parts.dsErr = resolver.NewDNSSECResolver(ctx, cfg.DNSSEC, parts.upstreamTree)

	return parts
}

// BenchmarkNewQueryResolverParts measures the construction of the resolvers
// that make up the query chain. "sequential" builds them one after the other,
// "concurrent" is the production path, which builds the independent ones at the
// same time.
func BenchmarkNewQueryResolverParts(b *testing.B) {
	listServer := benchListServer(b)

	// The mock server is created without its constructor because that one
	// registers a Ginkgo cleanup, which is not available in a benchmark.
	mock := &resolver.MockUDPUpstreamServer{}
	mock.WithAnswerRR("example.com. 123 IN A 192.0.2.1").WithDelay(benchSourceDelay)

	b.Cleanup(mock.Close)

	cfg, err := config.WithDefaults[config.Config]()
	if err != nil {
		b.Fatal(err)
	}

	cfg.QueryLog.Type = config.QueryLogTypeNone

	cfg.Upstreams.Groups = map[string][]config.Upstream{"default": {mock.Start()}}
	cfg.Upstreams.Timeout = config.Duration(5 * time.Second)

	cfg.Blocking.Loading.RefreshPeriod = -1
	cfg.Blocking.Denylists = map[string][]config.BytesSource{
		"gr": config.NewBytesSources(listServer.URL + "/denylist"),
	}

	ctx := context.Background()

	bootstrap, err := resolver.NewBootstrap(ctx, &cfg)
	if err != nil {
		b.Fatal(err)
	}

	b.Run("sequential", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			newQueryResolverPartsSequential(ctx, &cfg, bootstrap)
		}
	})

	b.Run("concurrent", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			newQueryResolverParts(ctx, &cfg, bootstrap)
		}
	})
}
