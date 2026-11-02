package resolver

import (
	"context"
	"log/slog"
	"testing"

	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/util"

	"github.com/miekg/dns"
)

// loggingPassthrough performs the same logger lookup as a real resolver, then
// delegates without emitting a record. Keep its backend cost separate from the
// isolated plumbing benchmark below.
type loggingPassthrough struct {
	typed
	NextResolver
}

func (loggingPassthrough) IsEnabled() bool        { return true }
func (loggingPassthrough) LogConfig(*slog.Logger) {}

func (r *loggingPassthrough) Resolve(ctx context.Context, req *model.Request) (*model.Response, error) {
	ctx, logger := r.log(ctx)
	_ = logger // not used at Info level — this is the whole point

	return r.next.Resolve(ctx, req)
}

// BenchmarkChainLoggingPlumbing measures a 15-resolver chain at Info level,
// INCLUDING benchBackend's DNS response construction. Its total allocations are
// not an isolated measurement of logging overhead.
//
//	go test -run=^$ -bench=BenchmarkChainLoggingPlumbing -benchmem ./resolver/
func BenchmarkChainLoggingPlumbing(b *testing.B) {
	const chainDepth = 15 // ~ resolvers traversed on a cache miss

	// production default: Info — Debug/Trace disabled
	prev := log.GetLevel(log.Log())
	log.SetLevel(log.Log(), slog.LevelInfo)
	b.Cleanup(func() { log.SetLevel(log.Log(), prev) })

	resolvers := make([]Resolver, 0, chainDepth+1)
	for i := range chainDepth {
		resolvers = append(resolvers, &loggingPassthrough{typed: withType(chainNodeName(i))})
	}
	resolvers = append(resolvers, benchBackend{})

	sut := Chain(resolvers...)

	req := &model.Request{
		Req:      util.NewMsgWithQuestion("example.com.", dns.Type(dns.TypeA)),
		Protocol: model.RequestProtocolUDP,
	}

	// Seed the same request fields as the server. Context construction is outside
	// the timed loop in this complete-chain comparison.
	baseCtx := log.CtxWithAttrs(context.Background(),
		slog.String("req_id", "00000000-0000-0000-0000-000000000000"),
		slog.String("question", "A (example.com.)"),
		slog.String("client_ip", "192.168.1.100"),
	)

	b.ReportAllocs()

	for b.Loop() {
		_, _ = sut.Resolve(baseCtx, req)
	}
}

// BenchmarkResolverLoggingPlumbing isolates logging from DNS message/model
// construction, parsers, caches and networking. The first case reuses request
// attributes; the second includes their per-request attachment and question
// snapshot, but deliberately excludes UUID generation.
func BenchmarkResolverLoggingPlumbing(b *testing.B) {
	b.Cleanup(log.SaveSettings(log.Log()))
	log.SetLevel(log.Log(), slog.LevelInfo)
	resolvers := make([]typed, 15)
	for i := range resolvers {
		resolvers[i] = withType(chainNodeName(i))
	}
	questions := []dns.Question{{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}}
	base := context.Background()
	requestCtx := log.CtxWithAttrs(
		base,
		slog.String("req_id", "00000000-0000-0000-0000-000000000000"),
		slog.Any("question", util.LogQuestions(questions)),
		slog.String("client_ip", "192.168.1.100"),
	)
	b.Run("resolver_chain", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ctx := requestCtx
			for i := range resolvers {
				loggedCtx, logger := resolvers[i].log(ctx)
				ctx = loggedCtx
				logger.DebugContext(ctx, "disabled")
			}
		}
	})
	b.Run("request_and_chain", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			ctx := log.CtxWithAttrs(
				base,
				slog.String("req_id", "00000000-0000-0000-0000-000000000000"),
				slog.Any("question", util.LogQuestions(questions)),
				slog.String("client_ip", "192.168.1.100"),
			)
			for i := range resolvers {
				loggedCtx, logger := resolvers[i].log(ctx)
				ctx = loggedCtx
				logger.DebugContext(ctx, "disabled")
			}
		}
	})
}

// BenchmarkDebugFieldBuild_Avoided measures the per-call cost of building the
// answer log field that Enabled guards skip when Debug is disabled. It measures
// the old eager formatting control, NOT the now-guarded production path.
//
//	go test -run=^$ -bench=BenchmarkDebugFieldBuild_Avoided -benchmem ./resolver/
func BenchmarkDebugFieldBuild_Avoided(b *testing.B) {
	answer := []dns.RR{
		mustRR("example.com. 3600 IN A 1.2.3.4"),
		mustRR("example.com. 3600 IN A 5.6.7.8"),
	}

	b.ReportAllocs()

	for b.Loop() {
		_ = util.Obfuscate(util.AnswerToString(answer))
	}
}

func mustRR(s string) dns.RR {
	rr, err := dns.NewRR(s)
	if err != nil {
		panic(err)
	}

	return rr
}

func chainNodeName(i int) string {
	names := []string{
		"stats", "rate_limiting", "filtering", "fqdn_only", "client_names",
		"ede", "query_logging", "metrics", "custom_dns", "hosts_file",
		"rebinding", "blocking", "dnssec", "caching", "conditional",
	}

	return names[i%len(names)]
}
