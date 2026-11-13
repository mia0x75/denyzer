package dnssec

import (
	"testing"

	"github.com/miekg/dns"
)

func TestAnswerTerminal(t *testing.T) {
	t.Parallel()
	alias := func(name, target string) dns.RR {
		return &dns.CNAME{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET}, Target: target}
	}
	address := &dns.A{Hdr: dns.RR_Header{Name: "end.example.", Rrtype: dns.TypeA, Class: dns.ClassINET}}
	tests := []struct {
		name     string
		records  []dns.RR
		qtype    uint16
		terminal string
		complete bool
		valid    bool
	}{
		{"unordered chain", []dns.RR{address, alias("middle.example.", "end.example."), alias("ALIAS.EXAMPLE.", "Middle.Example.")}, dns.TypeA, "end.example.", true, true},
		{"alias only", []dns.RR{alias("alias.example.", "end.example.")}, dns.TypeA, "end.example.", false, true},
		{"direct CNAME query", []dns.RR{alias("alias.example.", "end.example.")}, dns.TypeCNAME, "alias.example.", true, true},
		{"cycle", []dns.RR{alias("alias.example.", "middle.example."), alias("middle.example.", "alias.example.")}, dns.TypeA, "alias.example.", false, false},
		{"conflicting aliases", []dns.RR{alias("alias.example.", "one.example."), alias("alias.example.", "two.example.")}, dns.TypeA, "alias.example.", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			name, complete, valid := answerTerminal(tt.records, dns.Question{Name: "alias.example.", Qtype: tt.qtype, Qclass: dns.ClassINET})
			if name != tt.terminal || complete != tt.complete || valid != tt.valid {
				t.Fatalf("got (%s, %t, %t), want (%s, %t, %t)", name, complete, valid, tt.terminal, tt.complete, tt.valid)
			}
		})
	}
}

func TestMergeValidationResults(t *testing.T) {
	t.Parallel()
	order := []ValidationResult{ValidationResultSecure, ValidationResultInsecure, ValidationResultIndeterminate, ValidationResultBogus}
	for i, a := range order {
		for j, b := range order {
			t.Run(a.String()+"/"+b.String(), func(t *testing.T) {
				t.Parallel()
				want := order[max(i, j)]
				if got := mergeValidationResults(a, b); got != want {
					t.Fatalf("got %s, want %s", got, want)
				}
			})
		}
	}
}

func TestNSEC3HashVector(t *testing.T) {
	t.Parallel()
	validator := &Validator{}
	for range 100 {
		got, err := validator.computeNSEC3Hash("EXAMPLE", dns.SHA1, "AABBCCDD", 12)
		if err != nil || got != "0P9MHAVEQVM6T7VBL5LOP2U3T2RP3TOM" {
			t.Fatalf("RFC 5155 vector: got %q, %v", got, err)
		}
	}
}
