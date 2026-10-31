package resolver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/mia0x75/denyzer/config"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("BlockingResolver startup", Label("blockingResolver"), func() {
	It("loads the denylist and the allowlist concurrently", func(ctx context.Context) {
		// The test server holds both requests until it has seen one of each. With
		// sequential loading the first request would wait for the second one that
		// is never issued, so the resolver construction only completes when the
		// two caches are loaded in parallel.
		var (
			mu       sync.Mutex
			seen     = map[string]bool{}
			bothSeen = make(chan struct{})
			once     sync.Once
		)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()

			seen[r.URL.Path] = true
			if seen["/denylist"] && seen["/allowlist"] {
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
			case "/allowlist":
				_, _ = fmt.Fprintln(w, "allowed.test")
			}
		}))
		DeferCleanup(server.Close)

		loading, err := config.WithDefaults[config.SourceLoading]()
		Expect(err).Should(Succeed())
		loading.RefreshPeriod = -1

		blockingCfg := config.Blocking{
			BlockType: "ZEROIP",
			BlockTTL:  config.Duration(time.Minute),
			Loading:   loading,
			Denylists: map[string][]config.BytesSource{
				"gr": config.NewBytesSources(server.URL + "/denylist"),
			},
			Allowlists: map[string][]config.BytesSource{
				"gr": config.NewBytesSources(server.URL + "/allowlist"),
			},
		}

		bootstrap, err := NewBootstrap(ctx, &config.Config{})
		Expect(err).Should(Succeed())

		done := make(chan error, 1)

		go func() {
			defer GinkgoRecover()

			_, err := NewBlockingResolver(ctx, blockingCfg, bootstrap)
			done <- err
		}()

		Eventually(done, "10s").Should(Receive(Succeed()))
	})
})
