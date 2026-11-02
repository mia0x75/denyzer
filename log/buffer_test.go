package log

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Buffer", func() {
	var (
		buffer *Buffer
		target *slog.Logger
		hook   *Recorder
	)

	BeforeEach(func() {
		buffer = NewBuffer()

		target, hook = NewMockLogger()
	})

	It("replays each entry with its level, message, fields and time, in order", func() {
		at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

		entry := buffer.Logger().With("prefix", "config")
		Expect(entry.Handler().Handle(context.Background(), slog.NewRecord(at, slog.LevelWarn, "first", 0))).Should(Succeed())
		buffer.Logger().Log(context.Background(), LevelTrace, "second")

		buffer.Replay(target)

		entries := hook.Records()
		Expect(entries).Should(HaveLen(2))

		Expect(entries[0].Level).Should(Equal(slog.LevelWarn))
		Expect(entries[0].Message).Should(Equal("first"))
		Expect(RecordFields(entries[0])).Should(Equal(map[string]any{"prefix": "config"}))
		Expect(entries[0].Time).Should(Equal(at))

		Expect(entries[1].Level).Should(Equal(LevelTrace))
		Expect(entries[1].Message).Should(Equal("second"))
	})

	It("replays only what the target's level allows", func() {
		hook.SetLevel(slog.LevelWarn)

		buffer.Logger().Debug("dropped")
		buffer.Logger().Error("kept")

		buffer.Replay(target)

		Expect(hook.Records()).Should(HaveLen(1))
		Expect(hook.LastRecord().Message).Should(Equal("kept"))
	})

	It("replays each entry once", func() {
		buffer.Logger().Info("once")

		buffer.Replay(target)
		buffer.Replay(target)

		Expect(hook.Records()).Should(HaveLen(1))
	})

	It("holds entries logged concurrently", func() {
		const count = 50

		var wg sync.WaitGroup

		for range count {
			wg.Go(func() {
				buffer.Logger().Info("concurrent")
			})
		}

		wg.Wait()

		buffer.Replay(target)

		Expect(hook.Records()).Should(HaveLen(count))
	})
})

func TestBufferSnapshotsAndIndentation(t *testing.T) {
	t.Parallel()
	for _, format := range []FormatType{FormatTypeText, FormatTypeJson} {
		t.Run(format.String(), func(t *testing.T) {
			t.Parallel()
			buffer := NewBuffer()
			values := map[string]any{"names": []string{"original"}}
			ctx := CtxWithAttrs(t.Context(), slog.String("req_id", "request"))
			at := time.Date(2026, 11, 2, 9, 46, 47, 0, time.UTC)
			record := slog.NewRecord(at, slog.LevelWarn, "buffered", 123)
			record.AddAttrs(slog.Any("data", values))
			WithIndent(SetPrefix(buffer.Logger(), "source"), "  ", func(logger *slog.Logger) {
				if err := logger.Handler().Handle(ctx, record); err != nil {
					t.Fatal(err)
				}
			})
			values["names"].([]string)[0] = "changed"
			cfg := DefaultConfig()
			cfg.Format = format
			var out bytes.Buffer
			target := NewLogger(cfg)
			SetOutput(target, &out)
			recorder, stop := Capture(target)
			t.Cleanup(stop)
			WithIndent(SetPrefix(target, "destination"), "  ", buffer.Replay)
			buffer.Replay(target)
			if len(recorder.Records()) != 1 {
				t.Fatal("buffer replay duplicated its records")
			}
			captured := recorder.LastRecord()
			if captured.Time != at || captured.PC != 123 || captured.Level != slog.LevelWarn {
				t.Fatal("buffer replay changed record metadata")
			}
			want := map[string]any{
				"prefix": "source", "req_id": "request", "data": map[string]any{"names": []string{"original"}},
			}
			if got := RecordFields(captured); !reflect.DeepEqual(got, want) {
				t.Fatalf("buffer replay changed captured fields: %#v", got)
			}
			if format == FormatTypeText {
				if !bytes.Contains(out.Bytes(), []byte("source:     buffered")) {
					t.Fatalf("nested replay indentation was lost: %s", out.String())
				}

				return
			}
			var rendered map[string]any
			if err := json.Unmarshal(out.Bytes(), &rendered); err != nil {
				t.Fatal(err)
			}
			if rendered["msg"] != "buffered" || rendered["time"] != at.Format(time.RFC3339) {
				t.Fatalf("JSON replay changed the message or timestamp: %#v", rendered)
			}
		})
	}
}

func TestBufferConcurrentReplay(t *testing.T) {
	t.Parallel()
	buffer := NewBuffer()
	buffer.Logger().Info("first")
	buffer.Logger().Info("second")
	transport := &blockingSyslog{started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(transport.release) })
	t.Cleanup(release)
	target := NewLogger(DefaultConfig())
	sink := managedSink(target)
	sink.replace(&outputSink{syslog: transport}, slog.LevelInfo)
	t.Cleanup(func() { SetOutput(target, io.Discard) })
	recorder, stop := Capture(target)
	t.Cleanup(stop)
	var wg sync.WaitGroup
	wg.Go(func() { buffer.Replay(target) })
	<-transport.started
	buffer.Logger().Info("third")
	wg.Go(func() { buffer.Replay(target) })
	release()
	wg.Wait()
	buffer.Replay(target)
	if got, want := recorder.Messages(), []string{"first", "second", "third"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("concurrent replay lost, duplicated or reordered records: %q", got)
	}
}
