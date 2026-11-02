package log

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/slogtest"
	"time"
)

type countedValue struct{ calls *atomic.Int32 }

func (v countedValue) LogValue() slog.Value {
	v.calls.Add(1)

	return slog.StringValue("resolved")
}

func TestHandlerDefersValues(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	logger, recorder := NewMockLogger()
	recorder.SetLevel(slog.LevelInfo)
	logger = logger.With("bound", countedValue{calls: &calls})
	ctx := context.WithValue(t.Context(), attrsKey{}, []slog.Attr{slog.Any("context", countedValue{calls: &calls})})
	logger.DebugContext(ctx, "disabled", "record", countedValue{calls: &calls})
	if calls.Load() != 0 {
		t.Fatal("a disabled event resolved its values")
	}

	logger.InfoContext(ctx, "enabled", "record", countedValue{calls: &calls})
	if calls.Load() != 3 {
		t.Fatalf("resolved %d values, want 3", calls.Load())
	}

	want := map[string]any{"context": "resolved", "bound": "resolved", "record": "resolved"}
	if got := RecordFields(recorder.LastRecord()); !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %#v, want %#v", got, want)
	}
}

func TestHandlerConformance(t *testing.T) {
	t.Parallel()
	logger, recorder := NewMockLogger()
	results := func() []map[string]any {
		records := recorder.Records()
		result := make([]map[string]any, len(records))
		for i, record := range records {
			fields := RecordFields(record)
			if !record.Time.IsZero() {
				fields["time"] = record.Time
			}
			fields["level"] = record.Level
			fields["msg"] = record.Message
			result[i] = fields
		}

		return result
	}
	if err := slogtest.TestHandler(logger.Handler(), results); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerScopesAndGroups(t *testing.T) {
	t.Parallel()
	logger, recorder := NewMockLogger()
	logger = logger.With("prefix", "earlier", "top", 1)
	logger = logger.With("prefix", "resolver").WithGroup("nested").With("bound", 2)
	logger = logger.WithGroup("deep").With("before", 3)
	logger.LogAttrs(
		t.Context(),
		slog.LevelInfo,
		"message",
		slog.Int("after", 4),
		slog.Group("", "inline", 5),
		slog.Group("empty"),
		slog.Attr{},
	)
	want := map[string]any{
		"prefix": "resolver", "top": int64(1),
		"nested": map[string]any{
			"bound": int64(2),
			"deep":  map[string]any{"before": int64(3), "after": int64(4), "inline": int64(5)},
		},
	}
	if got := RecordFields(recorder.LastRecord()); !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %#v, want %#v", got, want)
	}

	root, capture := NewMockLogger()
	left := root.With("left", 1)
	right := root.With("right", 2)
	left.Info("left")
	right.Info("right")
	root.Info("root")
	for i, want := range []map[string]any{{"left": int64(1)}, {"right": int64(2)}, {}} {
		if got := RecordFields(capture.Records()[i]); !reflect.DeepEqual(got, want) {
			t.Fatalf("derived logger %d fields = %#v, want %#v", i, got, want)
		}
	}
}

func TestHandlerOutput(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 46, 47, 0, time.UTC)
	tests := []struct {
		name      string
		level     slog.Level
		timestamp bool
		colors    bool
		want      string
	}{
		{name: "info", level: slog.LevelInfo, want: " INFO resolver:   message empty= number=2\n"},
		{name: "warn", level: slog.LevelWarn, want: " WARN resolver:   message empty= number=2\n"},
		{name: "trace", level: LevelTrace, want: "TRACE resolver:   message empty= number=2\n"},
		{name: "fatal", level: LevelFatal, want: "FATAL resolver:   message empty= number=2\n"},
		{name: "panic", level: LevelPanic, want: "PANIC resolver:   message empty= number=2\n"},
		{
			name: "timestamp", level: slog.LevelInfo, timestamp: true,
			want: "[2026-11-02 09:46:47]  INFO resolver:   message empty= number=2\n",
		},
		{
			name: "color", level: slog.LevelInfo, colors: true,
			want: "\x1b[0;32m INFO\x1b[0m\x1b[0;1;34m resolver:\x1b[0m   message " +
				"\x1b[0;32mempty\x1b[0m= \x1b[0;32mnumber\x1b[0m=2\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			sink := &outputSink{out: &out, timestamp: tt.timestamp, colors: tt.colors}
			record := slog.NewRecord(at, tt.level, "message", 0)
			record.AddAttrs(slog.String("prefix", "resolver"), slog.Int("number", 2), slog.String("empty", ""))
			if err := sink.Handle(t.Context(), record, "  "); err != nil {
				t.Fatal(err)
			}

			if got := out.String(); got != tt.want {
				t.Fatalf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandlerJSON(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	sink := &outputSink{out: &out, format: FormatTypeJson, timestamp: false}
	record := slog.NewRecord(time.Date(2026, 11, 2, 9, 46, 47, 0, time.UTC), slog.LevelWarn, "message", 0)
	record.AddAttrs(
		slog.String("prefix", "resolver"), slog.String("time", "field time"), slog.String("level", "field level"),
		slog.String("msg", "field message"), slog.Any("error", errors.New("boom")),
	)
	if err := sink.Handle(t.Context(), record, "ignored indent"); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	want := map[string]any{
		"time": "2026-11-02T09:46:47Z", "level": "warning", "msg": "message",
		"prefix": "resolver", "error": "boom", "fields.time": "field time",
		"fields.level": "field level", "fields.msg": "field message",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON = %#v, want %#v", got, want)
	}
}
