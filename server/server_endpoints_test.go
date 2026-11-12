package server

import (
	"fmt"
	"net"
	"testing"

	"github.com/miekg/dns"
)

func TestGetSmallestTTLFromAnswer(t *testing.T) {
	tests := []struct {
		name string
		ttls []uint32
		want uint32
	}{
		{"empty answer", nil, 0},
		{"single zero", []uint32{0}, 0},
		{"zero first", []uint32{0, 60}, 0},
		{"zero last", []uint32{60, 0}, 0},
		{"zero in middle", []uint32{60, 0, 30}, 0},
		{"positive", []uint32{60, 30, 120}, 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := new(dns.Msg)
			for i, ttl := range tt.ttls {
				msg.Answer = append(msg.Answer, &dns.A{
					Hdr: dns.RR_Header{
						Name:   fmt.Sprintf("r%d.example.", i),
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    ttl,
					},
					A: net.ParseIP("192.0.2.1"),
				})
			}

			if got := getSmallestTTLFromAnswer(msg); got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}
