package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/util"
	"github.com/miekg/dns"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var systemResolverBootstrap = &Bootstrap{
	dialer:       newMockDialer(),
	configurable: withConfig(newBootstrapConfig(&config.Config{Upstreams: defaultUpstreamsConfig})),
}

var _ = Describe("Resolver", func() {
	Describe("Chains", func() {
		var (
			r1 ChainedResolver
			r2 ChainedResolver
			r3 ChainedResolver
			r4 Resolver
		)

		BeforeEach(func() {
			r1 = &mockResolver{}
			r2 = &mockResolver{}
			r3 = &mockResolver{}
			r4 = &NoOpResolver{}
		})

		Describe("Chain", func() {
			It("should create a chain iterable using `GetNext`", func() {
				ch := Chain(r1, r2, r3, r4)
				Expect(ch).ShouldNot(BeNil())
				Expect(ch).Should(Equal(r1))
				Expect(r1.GetNext()).Should(Equal(r2))
				Expect(r2.GetNext()).Should(Equal(r3))
				Expect(r3.GetNext()).Should(Equal(r4))
			})

			It("should not link a final ChainedResolver", func() {
				ch := Chain(r1, r2)
				Expect(ch).ShouldNot(BeNil())

				Expect(r1.GetNext()).Should(Equal(r2))
				Expect(r2.GetNext()).Should(BeNil())
			})
		})

		Describe("GetFromChainWithType", func() {
			It("should return resolver with type", func() {
				ch := Chain(&CustomDNSResolver{}, &BlockingResolver{})
				res, err := GetFromChainWithType[*BlockingResolver](ch)
				var expectedResolver *BlockingResolver
				Expect(err).Should(Succeed())
				Expect(res).Should(BeAssignableToTypeOf(expectedResolver))
			})
			It("should fail if chain does not contain the desired type", func() {
				ch := Chain(&CustomDNSResolver{}, &BlockingResolver{})
				_, err := GetFromChainWithType[*FilteringResolver](ch)

				Expect(err).Should(HaveOccurred())
			})
		})

		Describe("ForEach", func() {
			It("should iterate on all resolvers in the chain", func() {
				ch := Chain(r1, r2, r3, r4)
				Expect(ch).ShouldNot(BeNil())

				var itResult []Resolver

				ForEach(ch, func(r Resolver) {
					itResult = append(itResult, r)
				})

				Expect(itResult).ShouldNot(BeEmpty())
				Expect(itResult).Should(Equal([]Resolver{r1, r2, r3, r4}))
			})
		})

		Describe("LogResolverConfig", func() {
			It("should call the resolver's `LogConfig`", func() {
				logger := log.Log()

				m := &mockResolver{}
				m.On("IsEnabled").Return(true)
				m.On("LogConfig")

				LogResolverConfig(m, logger)

				m.AssertExpectations(GinkgoT())
			})

			When("the resolver is disabled", func() {
				It("should not call the resolver's `LogConfig`", func() {
					logger := log.Log()

					m := &mockResolver{}
					m.On("IsEnabled").Return(false)

					LogResolverConfig(m, logger)

					m.AssertExpectations(GinkgoT())
				})
			})
		})
	})

	Describe("Name", func() {
		var (
			ctx      context.Context
			cancelFn context.CancelFunc
		)
		BeforeEach(func() {
			ctx, cancelFn = context.WithCancel(context.Background())
			DeferCleanup(cancelFn)
		})
		When("'Name' is called", func() {
			It("should return resolver name", func() {
				br, _ := NewBlockingResolver(ctx, config.Blocking{BlockType: "zeroIP"}, systemResolverBootstrap)
				name := Name(br)
				Expect(name).Should(Equal("blocking"))
			})
		})
		// Note: RewriterResolver test removed as it's no longer used in the resolver chain
	})
})

func expectValidResolverType(sut Resolver) {
	By("it must not contain spaces", func() {
		Expect(sut.Type()).ShouldNot(ContainSubstring(" "))
	})
	By("it must be lower case", func() {
		Expect(sut.Type()).Should(Equal(strings.ToLower(sut.Type())))
	})
	By("it must not contain 'resolver'", func() {
		Expect(sut.Type()).ShouldNot(ContainSubstring("resolver"))
	})
}

func TestTypedLoggingRequestFields(t *testing.T) {
	t.Cleanup(log.SaveSettings(log.Log()))
	privacy := util.LogPrivacy.Load()
	util.LogPrivacy.Store(false)
	t.Cleanup(func() { util.LogPrivacy.Store(privacy) })
	log.SetOutput(log.Log(), io.Discard)
	log.SetLevel(log.Log(), slog.LevelDebug)
	recorder, stop := log.Capture(log.Log())
	t.Cleanup(stop)
	r := withType("upstream", slog.String(logFieldUpstream, "tcp+udp:192.0.2.1"))
	questions := []dns.Question{{Name: "original.example.com.", Qtype: dns.TypeA}}
	ctx := log.CtxWithAttrs(
		t.Context(),
		slog.String("req_id", "first"),
		slog.Any("question", util.LogQuestions(questions)),
		slog.String("client_ip", "192.0.2.10"),
		slog.String("client_names", "client-1"),
	)
	questions[0].Name = "rewritten.example.net."
	loggedCtx, logger := r.log(ctx)
	if loggedCtx != ctx || logger != r.logger {
		t.Fatal("the normal request path derived a context or logger")
	}
	logger.DebugContext(loggedCtx, "first request", "domain", "rewritten.example.net")
	want := map[string]any{
		"prefix": "upstream", "upstream": "tcp+udp:192.0.2.1", "req_id": "first",
		"question": "A (original.example.com.)", "client_ip": "192.0.2.10",
		"client_names": "client-1", "domain": "rewritten.example.net",
	}
	if got := log.RecordFields(recorder.LastRecord()); !reflect.DeepEqual(got, want) {
		t.Fatalf("request fields = %#v, want %#v", got, want)
	}
	secondCtx := log.CtxWithAttrs(t.Context(), slog.String("req_id", "second"))
	_, secondLogger := r.log(secondCtx)
	if secondLogger != logger {
		t.Fatal("request loggers are not reusable")
	}
	secondLogger.InfoContext(secondCtx, "second request")
	want = map[string]any{"prefix": "upstream", "upstream": "tcp+udp:192.0.2.1", "req_id": "second"}
	if got := log.RecordFields(recorder.LastRecord()); !reflect.DeepEqual(got, want) {
		t.Fatalf("request fields leaked between requests: %#v", got)
	}
}

func TestTypedLoggingExplicitLogger(t *testing.T) {
	t.Parallel()
	root, recorder := log.NewMockLogger()
	root = log.SetPrefix(root.With("initialization", "buffered"), "config")
	ctx, _ := log.NewCtx(t.Context(), root)
	ctx = log.CtxWithAttrs(ctx, slog.String("req_id", "request"))
	first := withType("caching")
	second := withType("upstream", slog.String(logFieldUpstream, "192.0.2.1"))
	_, logger := first.log(ctx)
	logger.InfoContext(ctx, "first")
	_, logger = second.log(ctx)
	logger.InfoContext(ctx, "second")
	want := map[string]any{
		"initialization": "buffered", "req_id": "request", "prefix": "upstream", "upstream": "192.0.2.1",
	}
	if got := log.RecordFields(recorder.LastRecord()); !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit logger lost fields or accumulated prefixes: %#v", got)
	}
	root.InfoContext(ctx, "root")
	if got := log.RecordFields(recorder.LastRecord())["prefix"]; got != "config" {
		t.Fatalf("resolver changed its parent logger's prefix: %v", got)
	}
}

func TestTypedLoggingReconfiguration(t *testing.T) {
	t.Cleanup(log.SaveSettings(log.Log()))
	r := withType("caching")
	ctx := log.CtxWithAttrs(t.Context(), slog.String("req_id", "request"))
	_, logger := r.log(ctx)
	global := log.Log()
	var first, second bytes.Buffer
	log.SetOutput(global, &first)
	log.SetLevel(global, slog.LevelInfo)
	logger.DebugContext(ctx, "disabled")
	if first.Len() != 0 {
		t.Fatal("a resolver ignored the current level")
	}
	cfg := log.DefaultConfig()
	cfg.Format = log.FormatTypeJson
	cfg.Level = log.Level(slog.LevelDebug)
	log.Configure(cfg)
	log.SetOutput(global, &second)
	logger.DebugContext(ctx, "enabled")
	if log.Log() != global || first.Len() != 0 {
		t.Fatal("reconfiguration replaced the global logger or used the old output")
	}
	var fields map[string]any
	if err := json.Unmarshal(second.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["prefix"] != "caching" || fields["req_id"] != "request" || fields["msg"] != "enabled" {
		t.Fatalf("existing resolver did not follow reconfiguration: %#v", fields)
	}
}

func TestTypedLoggingDisabledAllocations(t *testing.T) {
	t.Cleanup(log.SaveSettings(log.Log()))
	log.SetLevel(log.Log(), slog.LevelInfo)
	r := withType("upstream", slog.String(logFieldUpstream, "192.0.2.1"))
	ctx := log.CtxWithAttrs(t.Context(), slog.String("req_id", "request"))
	if allocs := testing.AllocsPerRun(1000, func() {
		loggedCtx, logger := r.log(ctx)
		logger.DebugContext(loggedCtx, "disabled")
	}); allocs != 0 {
		t.Fatalf("disabled resolver plumbing allocated %g times per call", allocs)
	}
}
