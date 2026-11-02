package log

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
)

type switchSink struct {
	level slog.LevelVar
	mu    sync.RWMutex

	output    *outputSink
	recorders []*Recorder
	cfg       Config
}

func (s *switchSink) Enabled(_ context.Context, level slog.Level) bool {
	return level >= s.level.Level()
}

func (s *switchSink) Handle(ctx context.Context, record slog.Record, indent string) error {
	// Hold the read lock through the write: switching waits for in-flight writes
	// before closing a replaced syslog connection, including derived loggers.
	s.mu.RLock()
	defer s.mu.RUnlock()

	if !s.Enabled(ctx, record.Level) {
		return nil
	}
	if s.output == nil && len(s.recorders) == 0 {
		return nil
	}

	// Resolve deferred values once so capture and output see the same event,
	// even when a LogValuer reads changing state (such as the privacy setting).
	attrs := recordAttrs(record)
	for _, recorder := range s.recorders {
		if recorder.Enabled(ctx, record.Level) {
			recorder.capture(record, attrs)
		}
	}

	if s.output == nil {
		return nil
	}

	if err := s.output.handleAttrs(record, attrs, indent); err != nil {
		// slog intentionally ignores handler errors. Keep output failures visible,
		// without recursively trying to use the failing output itself.
		fmt.Fprintf(os.Stderr, "Failed to write to log, %v\n", err)

		return err
	}

	return nil
}

func (s *switchSink) replace(output *outputSink, level slog.Level) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replaceLocked(output, level)
}

func (s *switchSink) replaceLocked(output *outputSink, level slog.Level) {
	old := s.output
	s.output = output
	s.level.Set(level)
	if old != nil && old != output {
		if err := old.close(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to close old syslog output, %v\n", err)
		}
	}
}
