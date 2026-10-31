package resolver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mia0x75/denyzer/config"
)

// benchUpstreamDelay is the artificial latency every mock upstream takes to
// answer the initialization test query. Upstream initialization is dominated by
// that test query, so the benchmark measures how well the initialization is
// parallelized.
const benchUpstreamDelay = 25 * time.Millisecond

// benchUpstreamCount is the number of upstreams (or upstream groups) the
// initialization benchmarks use.
const benchUpstreamCount = 4

// benchMockUpstreams starts n mock upstreams that answer the initialization
// test query after benchUpstreamDelay.
//
// The mock server is created without its constructor because that one registers
// a Ginkgo cleanup, which is not available in a benchmark.
func benchMockUpstreams(tb testing.TB, n int) []config.Upstream {
	tb.Helper()

	upstreams := make([]config.Upstream, 0, n)

	for range n {
		srv := &MockUDPUpstreamServer{}
		srv.WithAnswerRR("example.com 123 IN A 123.124.122.122").WithDelay(benchUpstreamDelay)
		tb.Cleanup(srv.Close)

		upstreams = append(upstreams, srv.Start())
	}

	return upstreams
}

// benchUpstreamsConfig returns an upstream configuration that uses the given
// upstreams as the default group.
func benchUpstreamsConfig(tb testing.TB, upstreams []config.Upstream) config.Upstreams {
	tb.Helper()

	cfg, err := config.WithDefaults[config.Upstreams]()
	if err != nil {
		tb.Fatal(err)
	}

	cfg.Timeout = config.Duration(5 * time.Second)
	cfg.Strategy = config.UpstreamStrategyParallelBest
	cfg.Groups = config.UpstreamGroups{upstreamDefaultCfgName: upstreams}

	return cfg
}

// createGroupResolversSequential is the upstream-at-a-time implementation the
// concurrent one replaced. It is the reference of the benchmark below.
func createGroupResolversSequential(
	ctx context.Context, cfg config.UpstreamGroup, bootstrap *Bootstrap,
) ([]*upstreamResolverStatus, error) {
	upstreams := cfg.GroupUpstreams()
	resolvers := make([]*upstreamResolverStatus, 0, len(upstreams))

	for _, upstream := range upstreams {
		resolver, err := NewUpstreamResolver(ctx, newUpstreamConfig(upstream, cfg.Upstreams), bootstrap)
		if err != nil {
			continue // err was already logged
		}

		resolvers = append(resolvers, newUpstreamResolverStatus(resolver))
	}

	if len(resolvers) == 0 {
		return nil, fmt.Errorf("no valid upstream for group %s", cfg.Name)
	}

	return resolvers, nil
}

// createUpstreamBranchesSequential is the group-at-a-time implementation the
// concurrent one replaced. It is the reference of the benchmark below.
func createUpstreamBranchesSequential(
	ctx context.Context, cfg config.Upstreams, bootstrap *Bootstrap,
) (map[string]Resolver, error) {
	branches := make(map[string]Resolver, len(cfg.Groups))
	errs := make([]error, 0, len(cfg.Groups))

	for group, upstreams := range cfg.Groups {
		var (
			upstream Resolver
			err      error
		)

		groupConfig := config.NewUpstreamGroup(group, cfg, upstreams)

		switch cfg.Strategy {
		case config.UpstreamStrategyParallelBest, config.UpstreamStrategyRandom:
			upstream, err = NewParallelBestResolver(ctx, groupConfig, bootstrap)
		case config.UpstreamStrategyStrict:
			upstream, err = NewStrictResolver(ctx, groupConfig, bootstrap)
		}

		if err != nil {
			errs = append(errs, fmt.Errorf("group %s: %w", group, err))

			continue
		}

		branches[group] = upstream
	}

	if len(errs) != 0 {
		return nil, errors.Join(errs...)
	}

	return branches, nil
}

// BenchmarkCreateGroupResolvers measures the initialization of the upstreams of
// a single group. "sequential" initializes them one after the other,
// "concurrent" is the production path, which initializes them at the same time.
func BenchmarkCreateGroupResolvers(b *testing.B) {
	upstreams := benchMockUpstreams(b, benchUpstreamCount)
	cfg := benchUpstreamsConfig(b, upstreams)
	groupCfg := config.NewUpstreamGroup(upstreamDefaultCfgName, cfg, upstreams)

	ctx := context.Background()

	b.Run("sequential", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			if _, err := createGroupResolversSequential(ctx, groupCfg, nil); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("concurrent", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			if _, err := createGroupResolvers(ctx, groupCfg, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkCreateUpstreamBranches measures the initialization of the upstream
// groups. "sequential" initializes the groups one after the other,
// "concurrent" is the production path, which initializes them at the same time.
func BenchmarkCreateUpstreamBranches(b *testing.B) {
	upstreams := benchMockUpstreams(b, benchUpstreamCount)

	cfg := benchUpstreamsConfig(b, upstreams)
	cfg.Groups = config.UpstreamGroups{}

	for i := range benchUpstreamCount {
		cfg.Groups[fmt.Sprintf("group-%d", i)] = upstreams[i : i+1]
	}

	ctx := context.Background()

	b.Run("sequential", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			if _, err := createUpstreamBranchesSequential(ctx, cfg, nil); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("concurrent", func(b *testing.B) {
		b.ReportAllocs()

		for b.Loop() {
			if _, err := createUpstreamBranches(ctx, cfg, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
