package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func TestLoggerTextCompatibility(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	at := time.Date(2026, 11, 2, 9, 46, 47, 0, time.UTC)
	tests := []struct {
		name      string
		timestamp bool
		prefix    string
		message   string
		want      string
	}{
		{
			name: "timestamp and prefix", timestamp: true, prefix: "resolver", message: "resolved query",
			want: "[2026-11-02 09:46:47]  INFO resolver: resolved query empty= upstream=1.1.1.1\n",
		},
		{
			name: "without timestamp", prefix: "resolver", message: "resolved query",
			want: " INFO resolver: resolved query empty= upstream=1.1.1.1\n",
		},
		{
			name: "without prefix", timestamp: true, message: "resolved query",
			want: "[2026-11-02 09:46:47]  INFO resolved query empty= upstream=1.1.1.1\n",
		},
		{
			name: "message prefix", message: "[legacy] resolved query",
			want: " INFO legacy: resolved query empty= upstream=1.1.1.1\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Timestamp = tt.timestamp
			var out bytes.Buffer
			logger := NewLogger(cfg)
			SetOutput(logger, &out)
			entry := logger.With("upstream", "1.1.1.1", "empty", "")
			if tt.prefix != "" {
				entry = SetPrefix(entry, tt.prefix)
			}
			if err := entry.Handler().Handle(t.Context(), slog.NewRecord(at, slog.LevelInfo, tt.message, 0)); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.want {
				t.Fatalf("text output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoggerJSONCompatibility(t *testing.T) {
	// The historical JSON formatter always includes time, even with timestamp:false.
	at := time.Date(2026, 11, 2, 9, 46, 47, 0, time.FixedZone("test", 8*60*60))
	cfg := DefaultConfig()
	cfg.Format = FormatTypeJson
	cfg.Timestamp = false
	var out bytes.Buffer
	logger := NewLogger(cfg)
	SetOutput(logger, &out)
	entry := logger.With(
		"prefix", "resolver", "error", errors.New("boom"), "empty", "", "count", 2,
		"msg", "field message", "level", "field level", "time", "field time",
	)
	if err := entry.Handler().Handle(t.Context(), slog.NewRecord(at, slog.LevelWarn, "slow response", 0)); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"time": "2026-11-02T09:46:47+08:00", "level": "warning", "msg": "slow response",
		"prefix": "resolver", "error": "boom", "empty": "", "count": float64(2),
		"fields.msg": "field message", "fields.level": "field level", "fields.time": "field time",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON output = %#v, want %#v", got, want)
	}
}

func TestLoggerPrefixAndIndentCompatibility(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, format := range []FormatType{FormatTypeText, FormatTypeJson} {
		t.Run(format.String(), func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Timestamp = false
			cfg.Format = format
			var out bytes.Buffer
			logger := NewLogger(cfg)
			SetOutput(logger, &out)
			entry := SetPrefix(logger.With("prefix", "earlier"), "blocking")
			entry = WithPrefix(entry, "client_id_cache")
			WithIndent(entry, "  ", func(indented *slog.Logger) {
				WithIndent(indented, "  ", func(nested *slog.Logger) { nested.Info("nested") })
			})
			entry.Info("plain")
			if format == FormatTypeText {
				want := " INFO blocking.client_id_cache:     nested\n INFO blocking.client_id_cache: plain\n"
				if got := out.String(); got != want {
					t.Fatalf("indented text = %q, want %q", got, want)
				}

				return
			}
			decoder := json.NewDecoder(&out)
			for _, message := range []string{"nested", "plain"} {
				var record map[string]any
				if err := decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				if record["prefix"] != "blocking.client_id_cache" || record["msg"] != message {
					t.Fatalf("JSON indentation changed message or prefix: %#v", record)
				}
			}
		})
	}
}
