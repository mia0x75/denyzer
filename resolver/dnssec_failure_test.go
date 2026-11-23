package resolver

import (
	"testing"

	"github.com/mia0x75/denyzer/model"
	"github.com/miekg/dns"
)

func TestFailureCacheKey(t *testing.T) {
	t.Parallel()
	request := func(name string) *model.Request {
		msg := new(dns.Msg)
		msg.SetQuestion(name, dns.TypeA)
		msg.SetEdns0(ednsUDPSize, true)

		return &model.Request{Req: msg}
	}
	first := request("EXAMPLE.TEST.")
	first.Req.Id = 1
	second := request("example.test.")
	second.Req.Id = 2
	key := failureCacheKey(first, "view", nil)
	if key == "" || key != failureCacheKey(second, "view", nil) {
		t.Fatal("message ID and query letter case must not partition failures")
	}
	for _, tt := range []struct {
		name   string
		change func(*dns.Msg)
	}{
		{"checking disabled", func(msg *dns.Msg) { msg.CheckingDisabled = true }},
		{"recursion disabled", func(msg *dns.Msg) { msg.RecursionDesired = false }},
		{"query class", func(msg *dns.Msg) { msg.Question[0].Qclass = dns.ClassCHAOS }},
		{"EDNS version", func(msg *dns.Msg) { msg.IsEdns0().SetVersion(1) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			query := request("example.test.")
			tt.change(query.Req)
			if failureCacheKey(query, "view", nil) == key {
				t.Fatal("different upstream query must have an independent failure")
			}
		})
	}
	if failureCacheKey(second, "other", nil) == key {
		t.Fatal("upstream views must not share failures")
	}
}
