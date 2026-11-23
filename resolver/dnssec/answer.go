package dnssec

import (
	"strings"

	"github.com/miekg/dns"
)

// answerTerminal follows owner names because upstreams need not order alias RRsets.
func answerTerminal(records []dns.RR, question dns.Question) (name string, complete, valid bool) {
	name = dns.CanonicalName(question.Name)
	seen := make(map[string]bool)
	for !seen[name] {
		seen[name] = true
		target := ""
		for _, rr := range records {
			if rr.Header().Class != question.Qclass || !strings.EqualFold(rr.Header().Name, name) {
				continue
			}
			if rr.Header().Rrtype == question.Qtype {
				complete = true
			}
			if alias, ok := rr.(*dns.CNAME); ok && question.Qtype != dns.TypeCNAME {
				next := dns.CanonicalName(alias.Target)
				if target != "" && target != next {
					return name, false, false
				}
				target = next
			}
		}
		if target == "" {
			return name, complete, true
		}
		if complete {
			return name, false, false
		}
		name = target
	}

	return name, false, false
}
