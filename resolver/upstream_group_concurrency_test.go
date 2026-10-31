package resolver

import (
	"context"
	"time"

	"github.com/mia0x75/denyzer/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// upstreamInitDelay is the artificial latency every mock upstream takes to
// answer the initialisation test query. It is long enough to dominate the
// initialisation time, so a sequential implementation is clearly slower than a
// concurrent one.
const upstreamInitDelay = 200 * time.Millisecond

// upstreamInitBudget is the upper bound for initialising several delayed
// upstreams concurrently. Sequential initialisation needs at least
// upstreamInitDelay per upstream, so this budget only passes when the
// upstreams are initialised at the same time.
const upstreamInitBudget = 2 * upstreamInitDelay

// startDelayedMockUpstream starts a mock upstream that answers the
// initialisation test query after upstreamInitDelay.
func startDelayedMockUpstream() config.Upstream {
	mock := NewMockUDPUpstreamServer().
		WithAnswerRR("example.com 123 IN A 123.124.122.122").
		WithDelay(upstreamInitDelay)

	return mock.Start()
}

var _ = Describe("Upstream initialization concurrency", Label("upstreamResolver"), func() {
	var (
		ctx      context.Context
		cancelFn context.CancelFunc

		cfg config.Upstreams
	)

	BeforeEach(func() {
		ctx, cancelFn = context.WithCancel(context.Background())
		DeferCleanup(cancelFn)

		cfg = defaultUpstreamsConfig
		// The mock upstreams answer after upstreamInitDelay, so the upstream
		// timeout has to be larger than that delay.
		cfg.Timeout = config.Duration(5 * upstreamInitDelay)
	})

	When("a group contains multiple upstreams", func() {
		It("initializes them concurrently", func() {
			upstreams := []config.Upstream{
				startDelayedMockUpstream(),
				startDelayedMockUpstream(),
				startDelayedMockUpstream(),
			}

			groupCfg := config.NewUpstreamGroup("test-group", cfg, upstreams)

			start := time.Now()
			resolvers, err := createGroupResolvers(ctx, groupCfg, nil)
			elapsed := time.Since(start)

			Expect(err).ToNot(HaveOccurred())
			Expect(resolvers).To(HaveLen(len(upstreams)))
			Expect(elapsed).To(BeNumerically("<", upstreamInitBudget))
		})
	})

	When("several groups are configured", func() {
		It("initializes them concurrently", func() {
			cfg.Groups = config.UpstreamGroups{
				"group-a": {startDelayedMockUpstream()},
				"group-b": {startDelayedMockUpstream()},
				"group-c": {startDelayedMockUpstream()},
			}
			cfg.Strategy = config.UpstreamStrategyParallelBest

			start := time.Now()
			branches, err := createUpstreamBranches(ctx, cfg, nil)
			elapsed := time.Since(start)

			Expect(err).ToNot(HaveOccurred())
			Expect(branches).To(HaveLen(len(cfg.Groups)))
			Expect(elapsed).To(BeNumerically("<", upstreamInitBudget))
		})
	})
})
