package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/resolver"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Query resolver construction", func() {
	It("constructs independent resolvers concurrently", func(ctx context.Context) {
		// The blocking resolver and the hosts file resolver download their sources
		// during construction. The server holds both requests until it has seen one
		// of each: with sequential construction the first one would wait for a
		// request that is never issued, so the chain only builds when the two
		// resolvers are constructed in parallel.
		var (
			mu       sync.Mutex
			seen     = map[string]bool{}
			bothSeen = make(chan struct{})
			once     sync.Once
		)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()

			seen[r.URL.Path] = true
			if seen["/denylist"] && seen["/hosts"] {
				once.Do(func() { close(bothSeen) })
			}

			mu.Unlock()

			select {
			case <-bothSeen:
			case <-r.Context().Done():
				return
			}

			switch r.URL.Path {
			case "/denylist":
				_, _ = fmt.Fprintln(w, "blocked.test")
			case "/hosts":
				_, _ = fmt.Fprintln(w, "192.0.2.10 hosts.test")
			}
		}))
		DeferCleanup(server.Close)

		upstream := resolver.NewMockUDPUpstreamServer().
			WithAnswerRR("example.com. 123 IN A 192.0.2.1").
			Start()

		cfg, err := config.WithDefaults[config.Config]()
		Expect(err).Should(Succeed())

		cfg.Upstreams.Groups = map[string][]config.Upstream{"default": {upstream}}
		cfg.Upstreams.Timeout = config.Duration(5 * time.Second)
		cfg.QueryLog.Type = config.QueryLogTypeNone

		cfg.Blocking.Loading.RefreshPeriod = -1
		cfg.Blocking.Denylists = map[string][]config.BytesSource{
			"gr": config.NewBytesSources(server.URL + "/denylist"),
		}

		cfg.HostsFile.Loading.RefreshPeriod = -1
		cfg.HostsFile.Sources = config.NewBytesSources(server.URL + "/hosts")

		bootstrap, err := resolver.NewBootstrap(ctx, &cfg)
		Expect(err).Should(Succeed())

		done := make(chan error, 1)

		go func() {
			defer GinkgoRecover()

			_, err := createQueryResolver(ctx, &cfg, bootstrap)
			done <- err
		}()

		Eventually(done, "10s").Should(Receive(Succeed()))
	})
})
