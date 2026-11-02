package log

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type blockingSyslog struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	closed  atomic.Bool
	late    atomic.Bool
	levels  []slog.Level
	lines   []string
}

func (s *blockingSyslog) writeRecord(level slog.Level, data []byte) error {
	if s.started != nil {
		s.once.Do(func() { close(s.started) })
		<-s.release
	}

	if s.closed.Load() {
		s.late.Store(true)
	}

	s.levels = append(s.levels, level)
	s.lines = append(s.lines, string(data))

	return nil
}

func (s *blockingSyslog) closeSyslog() error {
	s.closed.Store(true)

	return nil
}

func TestSwitchWaitsForWrites(t *testing.T) {
	t.Parallel()
	transport := &blockingSyslog{started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(transport.release) })
	defer release()
	sink := &switchSink{output: &outputSink{syslog: transport}}
	logger := slog.New(&recordHandler{sink: sink}).With("prefix", "resolver")
	writeDone := make(chan struct{})
	go func() {
		logger.Info("old output")
		close(writeDone)
	}()

	<-transport.started
	if sink.mu.TryLock() {
		sink.mu.Unlock()
		t.Fatal("in-flight write did not protect the old output")
	}

	var out bytes.Buffer
	switchDone := make(chan struct{})
	go func() {
		sink.replace(&outputSink{out: &out, format: FormatTypeJson}, slog.LevelDebug)
		close(switchDone)
	}()

	// RWMutex waits are not durable blocking operations in testing/synctest.
	select {
	case <-switchDone:
		t.Fatal("reconfigured with an old write still in flight")
	case <-time.After(20 * time.Millisecond):
	}

	if transport.closed.Load() {
		t.Fatal("closed the old syslog connection with a write in flight")
	}

	release()
	<-writeDone
	<-switchDone
	if !transport.closed.Load() || transport.late.Load() {
		t.Fatal("old output was not closed safely")
	}

	logger.Debug("new output")
	if !strings.Contains(out.String(), `"prefix":"resolver"`) || !strings.Contains(out.String(), `"msg":"new output"`) {
		t.Fatalf("derived logger did not follow reconfiguration: %s", out.String())
	}
}

func TestSwitchLevelThresholds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		level slog.Level
	}{
		{name: "trace", level: LevelTrace},
		{name: "debug", level: slog.LevelDebug},
		{name: "info", level: slog.LevelInfo},
		{name: "warn", level: slog.LevelWarn},
		{name: "error", level: slog.LevelError},
		{name: "fatal", level: LevelFatal},
		{name: "panic", level: LevelPanic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sink := new(switchSink)
			sink.level.Set(tt.level)
			for _, event := range tests {
				if got := sink.Enabled(t.Context(), event.level); got != (event.level >= tt.level) {
					t.Fatalf("threshold %s, event %s: Enabled = %v", tt.name, event.name, got)
				}
			}
		})
	}
}

type sequenceValue struct{ calls *atomic.Int32 }

func (v sequenceValue) LogValue() slog.Value {
	return slog.Int64Value(int64(v.calls.Add(1)))
}

type sequenceError struct{ calls *atomic.Int32 }

func (v *sequenceError) Error() string { return strconv.FormatInt(int64(v.calls.Add(1)), 10) }

func TestSwitchResolvesBeforeFanout(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	cfg.Format = FormatTypeJson
	logger := NewLogger(cfg)
	var out bytes.Buffer
	SetOutput(logger, &out)
	first, stopFirst := Capture(logger)
	t.Cleanup(stopFirst)
	second, stopSecond := Capture(logger)
	t.Cleanup(stopSecond)
	var calls atomic.Int32
	var errorCalls atomic.Int32
	value := sequenceValue{calls: &calls}
	ctx := CtxWithAttrs(t.Context(), slog.Any("context", value))
	logger = logger.With("bound", value).WithGroup("nested")
	logger.DebugContext(ctx, "disabled", "record", value)
	if calls.Load() != 0 || out.Len() != 0 || len(first.Records()) != 0 {
		t.Fatal("disabled logging resolved or captured a value")
	}
	logger.InfoContext(
		ctx,
		"fan-out",
		"record", value,
		slog.Group("deep", "value", value),
		"error", &sequenceError{calls: &errorCalls},
	)
	if got := calls.Load(); got != 4 {
		t.Fatalf("resolved %d original values, want 4 across all sinks", got)
	}
	if got := errorCalls.Load(); got != 1 {
		t.Fatalf("rendered the original error %d times, want once across all sinks", got)
	}
	fields := RecordFields(first.LastRecord())
	if got := RecordFields(second.LastRecord()); !reflect.DeepEqual(got, fields) {
		t.Fatalf("recorders saw different resolved fields: %#v != %#v", got, fields)
	}
	var rendered map[string]any
	if err := json.Unmarshal(out.Bytes(), &rendered); err != nil {
		t.Fatal(err)
	}
	delete(rendered, "time")
	delete(rendered, "level")
	delete(rendered, "msg")
	want, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("output and capture disagreed: %s != %s", got, want)
	}
	stopFirst()
	stopFirst()
	logger.Info("after detach")
	if len(first.Records()) != 1 || len(second.Records()) != 2 {
		t.Fatal("capture cleanup detached another recorder or was not idempotent")
	}
}

func TestConcurrentReconfiguration(t *testing.T) {
	t.Parallel()
	logger := NewLogger(DefaultConfig())
	var out bytes.Buffer
	SetOutput(logger, &out)
	recorder, stop := Capture(logger)
	t.Cleanup(stop)
	derived := SetPrefix(logger.With("static", "field"), "resolver").WithGroup("nested")
	sink := managedSink(logger)
	var wg sync.WaitGroup
	for worker := range 4 {
		wg.Go(func() {
			ctx := CtxWithAttrs(t.Context(), slog.Int("req_id", worker))
			for i := range 300 {
				derived.InfoContext(ctx, "concurrent", "iteration", i)
			}
		})
	}
	wg.Go(func() {
		for i := range 100 {
			cfg := DefaultConfig()
			if i%2 == 0 {
				cfg.Format = FormatTypeJson
			}
			// Exercise the same atomic format/output replacement used by
			// ConfigureLogger, without directing test logs to the real console.
			sink.mu.Lock()
			sink.cfg = *cfg
			sink.replaceLocked(streamOutput(cfg, &out), cfg.Level.Level())
			sink.mu.Unlock()
		}
	})
	wg.Go(func() {
		for i := range 100 {
			SetOutput(logger, &out)
			level := slog.LevelInfo
			if i%2 == 0 {
				level = slog.LevelWarn
			}
			SetLevel(logger, level)
		}
	})
	wg.Go(func() {
		for range 50 {
			temporary, cleanup := Capture(logger)
			derived.Info("temporary capture")
			_ = temporary.Records()
			_ = recorder.LastRecord()
			_ = recorder.Messages()
			cleanup()
			cleanup()
		}
	})
	wg.Wait()
	SetLevel(logger, slog.LevelInfo)
	derived.Info("after reconfiguration")
	records := recorder.Records()
	lines := 0
	scanner := bufio.NewScanner(&out)
	for scanner.Scan() {
		lines++
		assertReconfiguredLine(t, scanner.Bytes())
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != len(records) || recorder.LastRecord().Message != "after reconfiguration" {
		t.Fatalf("output/capture lost records: %d lines, %d records", lines, len(records))
	}
}

func assertReconfiguredLine(t *testing.T, line []byte) {
	t.Helper()
	if !bytes.HasPrefix(line, []byte("{")) {
		if !bytes.Contains(line, []byte("resolver:")) || !bytes.Contains(line, []byte("static=field")) {
			t.Fatalf("reconfiguration produced incomplete text: %q", line)
		}

		return
	}
	var fields map[string]any
	if err := json.Unmarshal(line, &fields); err != nil {
		t.Fatalf("reconfiguration produced incomplete JSON: %v", err)
	}
	if fields["prefix"] != "resolver" || fields["static"] != "field" {
		t.Fatalf("derived fields were lost during reconfiguration: %#v", fields)
	}
}

type shortLogWriter struct{}

func (shortLogWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

func TestOutputShortWrite(t *testing.T) {
	t.Parallel()
	sink := &outputSink{out: shortLogWriter{}}
	if err := sink.Handle(t.Context(), slog.NewRecord(time.Time{}, slog.LevelInfo, "message", 0), ""); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v, want %v", err, io.ErrShortWrite)
	}
}

type failingLogWriter struct {
	err   error
	short bool
	calls int
}

func (w *failingLogWriter) Write(data []byte) (int, error) {
	w.calls++
	if w.short {
		return len(data) - 1, nil
	}

	return 0, w.err
}

type failingSyslog struct {
	writeErr error
	closeErr error
	writes   int
	closes   int
}

func (s *failingSyslog) writeRecord(slog.Level, []byte) error {
	s.writes++

	return s.writeErr
}

func (s *failingSyslog) closeSyslog() error {
	s.closes++

	return s.closeErr
}

func TestSwitchReportsOutputFailures(t *testing.T) {
	const helperEnv = "DENYZER_TEST_OUTPUT_FAILURE"
	if mode := os.Getenv(helperEnv); mode != "" {
		runOutputFailureHelper(t, mode)

		return
	}
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"stream write", "short write", "syslog write", "syslog close", "buffer replay"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSwitchReportsOutputFailures$")
			cmd.Env = append(os.Environ(), helperEnv+"="+mode, "NO_COLOR=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("failure helper: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
			}
			prefix, want := "Failed to write to log, ", 2
			if mode == "syslog close" {
				prefix, want = "Failed to close old syslog output, ", 1
			}
			if got := strings.Count(stderr.String(), prefix); got != want {
				t.Fatalf("stderr reported %d failures, want %d: %s", got, want, stderr.String())
			}
		})
	}
}

func runOutputFailureHelper(t *testing.T, mode string) {
	t.Helper()
	logger := NewLogger(DefaultConfig())
	SetOutput(logger, io.Discard)
	recorder, stop := Capture(logger)
	defer stop()
	failure := errors.New("test output failure")
	writer := &failingLogWriter{err: failure, short: mode == "short write"}
	transport := &failingSyslog{writeErr: failure}
	switch mode {
	case "syslog write":
		managedSink(logger).replace(&outputSink{syslog: transport}, slog.LevelInfo)
	case "syslog close":
		transport.closeErr = failure
		managedSink(logger).replace(&outputSink{syslog: transport}, slog.LevelInfo)
		SetOutput(logger, io.Discard)
		logger.Info("after close failure")
		if transport.closes != 1 || recorder.LastRecord().Message != "after close failure" {
			t.Fatal("a close failure broke the replacement output")
		}

		return
	default:
		SetOutput(logger, writer)
	}
	if mode == "buffer replay" {
		buffer := NewBuffer()
		buffer.Logger().Info("first")
		buffer.Logger().Info("second")
		buffer.Replay(logger)
		buffer.Replay(logger)
		if writer.calls != 2 || !reflect.DeepEqual(recorder.Messages(), []string{"first", "second"}) {
			t.Fatal("failed buffer replay recursed, lost or repeated a record")
		}

		return
	}
	if mode == "short write" {
		failure = io.ErrShortWrite
	}
	record := slog.NewRecord(time.Time{}, slog.LevelInfo, "direct failure", 0)
	if err := logger.Handler().Handle(t.Context(), record); !errors.Is(err, failure) {
		t.Fatalf("handler error = %v, want %v", err, failure)
	}
	logger.Info("ignored handler error")
	if len(recorder.Records()) != 2 || writer.calls+transport.writes != 2 {
		t.Fatal("an output failure recursed or prevented capture")
	}
}
