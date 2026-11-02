package log

import (
	"io"
	"testing"
)

func BenchmarkLogger(b *testing.B) {
	for _, format := range []FormatType{FormatTypeText, FormatTypeJson} {
		b.Run(format.String(), func(b *testing.B) {
			cfg := DefaultConfig()
			cfg.Format = format
			logger := NewLogger(cfg)
			SetOutput(logger, io.Discard)
			entry := logger.With(
				"prefix", "resolver", "req_id", "b8f4c1e2-0000-1111-2222-333344445555",
				"question", "A example.com.", "client_ip", "192.168.178.1",
			)
			b.ReportAllocs()
			for b.Loop() {
				entry.Info("query resolved")
			}
		})
	}
}
