package server

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"uuid"

	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/util"
	"github.com/miekg/dns"
)

func TestNewRequestLoggingSnapshot(t *testing.T) {
	t.Cleanup(log.SaveSettings(log.Log()))
	privacy := util.LogPrivacy.Load()
	t.Cleanup(func() { util.LogPrivacy.Store(privacy) })
	util.LogPrivacy.Store(false)
	log.SetOutput(log.Log(), io.Discard)
	log.SetLevel(log.Log(), slog.LevelInfo)
	recorder, stop := log.Capture(log.Log())
	t.Cleanup(stop)
	msg := util.NewMsgWithQuestion("original.example.com.", dns.Type(dns.TypeA))
	ctx, request := newRequest(
		t.Context(),
		net.ParseIP("192.0.2.10"),
		"client-1",
		model.RequestProtocolUDP,
		msg,
	)
	if len(recorder.Records()) != 0 {
		t.Fatal("disabled ingress trace was emitted")
	}
	request.Req.Question[0].Name = "rewritten.example.net."
	request.Req.Question[0].Qtype = dns.TypeAAAA
	log.FromCtx(ctx).InfoContext(ctx, "later record")
	fields := log.RecordFields(recorder.LastRecord())
	if fields["question"] != "A (original.example.com.)" {
		t.Fatalf("question changed after the resolver mutated the request: %#v", fields)
	}
	id, ok := fields["req_id"].(string)
	if !ok {
		t.Fatalf("missing request ID: %#v", fields)
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed[6]>>4 != 7 {
		t.Fatalf("request ID is not UUID v7: %q, %v", id, err)
	}
	if ip, ok := fields["client_ip"].(net.IP); !ok || !ip.Equal(request.ClientIP) {
		t.Fatalf("client IP changed: %#v", fields)
	}
	log.SetLevel(log.Log(), log.LevelTrace)
	util.LogPrivacy.Store(true)
	_, _ = newRequest(t.Context(), request.ClientIP, "client-2", request.Protocol, msg)
	fields = log.RecordFields(recorder.LastRecord())
	if fields["client_id"] != "client-2" || fields["client_request_id"] != uint64(msg.Id) {
		t.Fatalf("ingress trace lost its fields: %#v", fields)
	}
	if question, ok := fields["question"].(string); !ok || strings.Contains(question, "rewritten") {
		t.Fatalf("ingress trace exposed a private question: %#v", fields)
	}
}
