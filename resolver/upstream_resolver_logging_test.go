package resolver

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/mia0x75/denyzer/log"
	"github.com/miekg/dns"
)

func TestUpstreamResponseLoggingDisabledAllocations(t *testing.T) {
	// AllocsPerRun controls GOMAXPROCS and cannot run in a parallel test.
	logger := log.SetPrefix(log.NewLogger(log.DefaultConfig()), upstreamResolverType)
	log.SetOutput(logger, io.Discard)
	r := &UpstreamResolver{}
	request := newRequest("example.com.", dns.Type(dns.TypeA))
	response := &dns.Msg{Answer: []dns.RR{
		&dns.A{A: net.ParseIP("192.0.2.1")},
		&dns.TXT{Txt: []string{"mutable answer"}},
	}}
	ctx := log.CtxWithAttrs(t.Context(), slog.String("req_id", "request"))
	ip := net.ParseIP("192.0.2.1")
	if allocs := testing.AllocsPerRun(1000, func() {
		r.logResponse(ctx, logger, request, response, ip, time.Millisecond)
	}); allocs != 0 {
		t.Fatalf("disabled answer logging allocated %g times per call", allocs)
	}
}
