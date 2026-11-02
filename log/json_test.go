package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"testing"
	"time"
)

func TestJSONValues(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 46, 47, 123456789, time.FixedZone("test", 8*60*60))
	tests := []struct {
		name  string
		attrs []slog.Attr
	}{
		{
			name: "primitives",
			attrs: []slog.Attr{
				slog.String("string", "A example.com."), slog.Bool("bool", true),
				slog.Int64("min", math.MinInt64), slog.Uint64("max", math.MaxUint64),
				slog.Duration("duration", -time.Second), slog.Time("time_value", at),
				slog.Float64("small", 1e-9), slog.Float64("large", 1e21),
				slog.Any("nil", nil), slog.Any("error", errors.New("boom")),
			},
		},
		{
			name: "collections and groups",
			attrs: []slog.Attr{
				slog.Any("bytes", []byte("binary")), slog.Any("array", [2]int{1, 2}),
				slog.Any("slice", []string{"first", "second"}),
				slog.Any("map", map[string]any{"b": 2, "a": "first"}),
				slog.Any("nil_slice", []string(nil)), slog.Any("nil_map", map[string]int(nil)),
				slog.Any("custom", json.RawMessage(`{"custom":true}`)),
				slog.Group("group", slog.String("z", "last"), slog.String("a", "first")),
				slog.Group("group", slog.Group("nested", slog.String("msg", "unchanged"))),
				slog.Group("empty"),
				{},
			},
		},
		{
			name: "reserved fields override earlier aliases",
			attrs: []slog.Attr{
				slog.String("fields.msg", "overridden"), slog.String("msg", "field message"),
				slog.String("fields.time", "overridden"), slog.String("time", "field time"),
				slog.String("fields.level", "overridden"), slog.String("level", "field level"),
			},
		},
		{
			name: "reserved fields override later aliases",
			attrs: []slog.Attr{
				slog.String("msg", "field message"), slog.String("fields.msg", "overridden"),
				slog.String("time", "field time"), slog.String("fields.time", "overridden"),
				slog.String("level", "field level"), slog.String("fields.level", "overridden"),
			},
		},
		{
			name: "escaping",
			attrs: []slog.Attr{
				slog.String("quotes\"and\\slashes", "\"\\\n\r\t\a\v\x00\x1f"),
				slog.String("HTML", "<script>&</script>"),
				slog.String("Unicode", "日志\u2028\u2029"),
				slog.String("invalid UTF-8\xff", "invalid\xfe\xff"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			record := slog.NewRecord(at, slog.LevelWarn, "message\n<&>日志", 0)
			record.AddAttrs(tt.attrs...)
			fields := RecordFields(record)
			for _, key := range []string{"time", "level", "msg"} {
				if value, found := fields[key]; found {
					fields["fields."+key] = value
					delete(fields, key)
				}
			}
			fields["time"] = at.Format(time.RFC3339)
			fields["level"] = "warning"
			fields["msg"] = record.Message
			var want bytes.Buffer
			if err := json.NewEncoder(&want).Encode(fields); err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			if err := renderJSON(&got, record, recordAttrs(record)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("JSON differs from encoding/json:\ngot  %s\nwant %s", got.String(), want.String())
			}
		})
	}
}

func TestJSONEncodingErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value any
	}{
		{name: "unsupported type", value: make(chan int)},
		{name: "NaN", value: math.NaN()},
		{name: "infinity", value: math.Inf(1)},
		{name: "invalid time", value: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "invalid raw JSON", value: json.RawMessage(`{`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			sink := &outputSink{out: &out, format: FormatTypeJson}
			record := slog.NewRecord(time.Time{}, slog.LevelInfo, "invalid", 0)
			record.AddAttrs(slog.Any("value", tt.value))
			if err := sink.Handle(t.Context(), record, ""); err == nil {
				t.Fatal("JSON encoding accepted an unsupported value")
			}
			if out.Len() != 0 {
				t.Fatalf("encoding failure wrote a partial record: %s", out.String())
			}
		})
	}
}

func FuzzJSONString(f *testing.F) {
	for _, seed := range []string{"", "A example.com.", "\"\\\n\x00<&>", "日志\u2028\u2029", "invalid\xff"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		want, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		writeJSONString(&got, value)
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("JSON string = %q, want %q", got.Bytes(), want)
		}
	})
}
