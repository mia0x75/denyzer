package resolver

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func BenchmarkBucketStoreAllowAt(b *testing.B) {
	for _, shared := range []bool{true, false} {
		name := "distinct_keys"
		if shared {
			name = "shared_key"
		}
		b.Run(name, func(b *testing.B) {
			s := newBucketStore(0, 1, b.N+1)
			now := time.Now()
			var next atomic.Int64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				key := "shared"
				if !shared {
					key = strconv.FormatInt(next.Add(1), 10)
				}
				for pb.Next() {
					_, _ = s.allowAt(key, now)
				}
			})
		})
	}
}
