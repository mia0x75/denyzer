package lists

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"

	"github.com/mia0x75/denyzer/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("List refresh memory bound", func() {
	It("rebuilds one group at a time when groupConcurrency is 1 while keeping old rules",
		func(ctx context.Context) {
			started := make(chan struct{}, 4)
			release := make(chan struct{})
			var refreshing atomic.Bool

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if refreshing.Load() {
					started <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					_, _ = fmt.Fprintln(w, "*.refreshed.test")
				}
				_, _ = fmt.Fprintln(w, "*.example.test")
			}))
			DeferCleanup(server.Close)
			DeferCleanup(func() { close(release) })

			cfg, err := config.WithDefaults[config.SourceLoading]()
			Expect(err).To(Succeed())
			cfg.RefreshPeriod = -1
			cfg.Strategy = config.InitStrategyFailOnError
			cfg.GroupConcurrency = 1
			cache, err := NewListCache(ctx, ListCacheTypeDenylist, cfg, map[string][]config.BytesSource{
				"one": config.NewBytesSources(server.URL),
				"two": config.NewBytesSources(server.URL),
			}, NewDownloader(cfg.Downloads, nil))
			Expect(err).To(Succeed())

			refreshing.Store(true)
			done := make(chan error, 2)
			for range 2 {
				go func() { done <- cache.Refresh(ctx) }()
			}

			for range 4 {
				Eventually(started).Should(Receive())
				Consistently(started, "50ms").ShouldNot(Receive())
				Expect(cache.Match("sub.example.test", []string{"one", "two"})).To(Equal(map[string]string{
					"one": "*.example.test", "two": "*.example.test",
				}))
				release <- struct{}{}
			}
			for range 2 {
				Eventually(done).Should(Receive(Succeed()))
			}
			Expect(cache.Match("sub.refreshed.test", []string{"one", "two"})).To(Equal(map[string]string{
				"one": "*.refreshed.test", "two": "*.refreshed.test",
			}))
		})
})

var _ = Describe("List group concurrency", func() {
	It("loads no more than groupConcurrency groups at once", func(ctx context.Context) {
		started := make(chan struct{}, 8)
		release := make(chan struct{})

		var releaseOnce sync.Once

		releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
		DeferCleanup(releaseAll)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started <- struct{}{}

			select {
			case <-release:
			case <-r.Context().Done():
				return
			}

			_, _ = fmt.Fprintln(w, "*.example.test")
		}))
		DeferCleanup(server.Close)

		cfg, err := config.WithDefaults[config.SourceLoading]()
		Expect(err).To(Succeed())
		cfg.RefreshPeriod = -1
		cfg.Strategy = config.InitStrategyFailOnError
		cfg.Concurrency = 8 // per-source limit must not throttle the group limit
		cfg.GroupConcurrency = 2

		groupSources := make(map[string][]config.BytesSource, 4)
		for i := range 4 {
			groupSources[fmt.Sprintf("gr%d", i)] = config.NewBytesSources(server.URL)
		}

		done := make(chan error, 1)

		go func() {
			defer GinkgoRecover()

			_, err := NewListCache(ctx, ListCacheTypeDenylist, cfg, groupSources, NewDownloader(cfg.Downloads, nil))
			done <- err
		}()

		// Exactly two groups start; the other two wait for a slot.
		Eventually(started).Should(Receive())
		Eventually(started).Should(Receive())
		Consistently(started, "100ms").ShouldNot(Receive())

		releaseAll()

		Eventually(started).Should(Receive())
		Eventually(started).Should(Receive())
		Eventually(done, "5s").Should(Receive(Succeed()))
	})

	It("loads groups sequentially when groupConcurrency is 1", func(ctx context.Context) {
		started := make(chan struct{}, 4)
		release := make(chan struct{})

		var releaseOnce sync.Once

		releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
		DeferCleanup(releaseAll)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started <- struct{}{}

			select {
			case <-release:
			case <-r.Context().Done():
				return
			}

			_, _ = fmt.Fprintln(w, "*.example.test")
		}))
		DeferCleanup(server.Close)

		cfg, err := config.WithDefaults[config.SourceLoading]()
		Expect(err).To(Succeed())
		cfg.RefreshPeriod = -1
		cfg.Strategy = config.InitStrategyFailOnError
		cfg.GroupConcurrency = 1

		groupSources := make(map[string][]config.BytesSource, 2)
		for i := range 2 {
			groupSources[fmt.Sprintf("gr%d", i)] = config.NewBytesSources(server.URL)
		}

		done := make(chan error, 1)

		go func() {
			defer GinkgoRecover()

			_, err := NewListCache(ctx, ListCacheTypeDenylist, cfg, groupSources, NewDownloader(cfg.Downloads, nil))
			done <- err
		}()

		Eventually(started).Should(Receive())
		Consistently(started, "100ms").ShouldNot(Receive())

		releaseAll()

		Eventually(started).Should(Receive())
		Eventually(done, "5s").Should(Receive(Succeed()))
	})
})
