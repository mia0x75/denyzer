package dnssec

import (
	"fmt"
	"testing"

	"github.com/miekg/dns"
)

func BenchmarkNSEC3Hash(b *testing.B) {
	for _, iterations := range []uint16{0, 150} {
		for _, varied := range []bool{false, true} {
			b.Run(fmt.Sprintf("iterations=%d/varied=%t", iterations, varied), func(b *testing.B) {
				validator := &Validator{}
				names := make([]string, 1024)
				for i := range names {
					names[i] = fmt.Sprintf("name-%d.example.", i)
				}
				i := 0
				b.ReportAllocs()
				for b.Loop() {
					if varied {
						i = (i + 1) % len(names)
					}
					if _, err := validator.computeNSEC3Hash(names[i], dns.SHA1, "AABBCCDD", iterations); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
