package resolver

import (
	"context"
	"io"
	"log/slog"
	"net"
	"reflect"
	"testing"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/model"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/mock"
)

func TestBootstrapLoggingOrigin(t *testing.T) {
	t.Cleanup(log.SaveSettings(log.Log()))
	log.SetOutput(log.Log(), io.Discard)
	log.SetLevel(log.Log(), slog.LevelDebug)
	recorder, stop := log.Capture(log.Log())
	t.Cleanup(stop)
	cfg, err := config.WithDefaults[config.Config]()
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnectIPVersion = config.IPVersionV4
	nested := withType("caching")
	backend := &mockResolver{ResolveFn: func(ctx context.Context, _ *model.Request) (*model.Response, error) {
		_, logger := nested.log(ctx)
		logger.DebugContext(ctx, "bootstrap refresh")

		return &model.Response{Res: &dns.Msg{Answer: []dns.RR{&dns.A{A: net.ParseIP("192.0.2.1")}}}}, nil
	}}
	backend.On("Resolve", mock.Anything).Return(nil, nil).Once()
	bootstrap := &Bootstrap{
		configurable: withConfig(newBootstrapConfig(&cfg)),
		typed:        withType("bootstrap"),
		resolver:     backend,
	}
	upstream := newUpstreamResolverUnchecked(newUpstreamConfig(config.Upstream{
		Net: config.NetProtocolTcpUdp, Host: "origin.example.com", Port: 53,
	}, cfg.Upstreams), bootstrap)
	ctx := log.CtxWithAttrs(t.Context(), slog.String("req_id", "request"))
	if _, err := bootstrap.UpstreamIPs(ctx, upstream); err != nil {
		t.Fatal(err)
	}
	backend.AssertExpectations(t)
	want := map[string]any{
		"req_id": "request", "prefix": "caching", "upstream": upstream.cfg.String(),
	}
	if got := log.RecordFields(recorder.LastRecord()); !reflect.DeepEqual(got, want) {
		t.Fatalf("nested bootstrap record lost the originating upstream: %#v", got)
	}
	log.FromCtx(ctx).InfoContext(ctx, "parent request")
	if got := log.RecordFields(recorder.LastRecord()); got["upstream"] != nil {
		t.Fatalf("bootstrap fields leaked into the parent request: %#v", got)
	}
}
