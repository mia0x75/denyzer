package log

import (
	"io"
	"log/slog"
	"slices"
	"sync"
)

func managedSink(logger *slog.Logger) *switchSink {
	if handler, ok := logger.Handler().(*recordHandler); ok {
		if sink, ok := handler.sink.(*switchSink); ok {
			return sink
		}
	}

	panic("logging configuration requires a logger created by log.NewLogger")
}

// SetLevel changes a managed logger's minimum level, including derived loggers.
func SetLevel(logger *slog.Logger, level slog.Level) {
	sink := managedSink(logger)
	sink.mu.Lock()
	sink.cfg.Level = Level(level)
	sink.level.Set(level)
	sink.mu.Unlock()
}

// GetLevel returns a managed logger's current minimum level.
func GetLevel(logger *slog.Logger) slog.Level { return managedSink(logger).level.Level() }

// SetOutput directs a managed logger to a stream, keeping its format and level.
// A replaced syslog connection is closed after its in-flight writes complete.
func SetOutput(logger *slog.Logger, out io.Writer) {
	sink := managedSink(logger)
	sink.mu.Lock()
	defer sink.mu.Unlock()

	sink.cfg.Target = TargetTypeStdout
	sink.replaceLocked(streamOutput(&sink.cfg, out), sink.level.Level())
}

// IsSilenced reports whether a managed logger has no visible output.
func IsSilenced(logger *slog.Logger) bool {
	sink := managedSink(logger)
	sink.mu.RLock()
	defer sink.mu.RUnlock()

	return sink.output == nil || sink.output.out == io.Discard
}

// Capture attaches a recorder without changing the output or level. The returned
// cleanup function detaches only this recorder, and is safe to call repeatedly.
func Capture(logger *slog.Logger) (*Recorder, func()) {
	sink := managedSink(logger)
	recorder := NewRecorder()
	sink.mu.Lock()
	sink.recorders = append(sink.recorders, recorder)
	sink.mu.Unlock()

	return recorder, sync.OnceFunc(func() {
		sink.mu.Lock()
		sink.recorders = slices.DeleteFunc(sink.recorders, func(r *Recorder) bool { return r == recorder })
		sink.mu.Unlock()
	})
}

// SaveSettings returns an idempotent function restoring a managed logger's
// settings and capture state. Syslog is redialed rather than reusing a connection
// that reconfiguration may have closed. This is useful for isolated tests.
func SaveSettings(logger *slog.Logger) func() {
	sink := managedSink(logger)
	sink.mu.RLock()
	cfg, level := sink.cfg, sink.level.Level()
	recorders := slices.Clone(sink.recorders)
	var saved *outputSink
	if output := sink.output; output != nil {
		saved = &outputSink{
			out: output.out, syslog: output.syslog, format: output.format,
			timestamp: output.timestamp, colors: output.colors,
		}
	}

	sink.mu.RUnlock()

	return sync.OnceFunc(func() {
		output := saved
		var err error
		if saved != nil && saved.syslog != nil {
			output, err = configuredOutput(&cfg)
		}

		sink.mu.Lock()
		sink.cfg, sink.recorders = cfg, recorders
		sink.replaceLocked(output, level)
		sink.mu.Unlock()
		if err != nil {
			logger.Error("can't restore syslog, using stderr instead: " + err.Error())
		}
	})
}
