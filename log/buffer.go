package log

import (
	"context"
	"log/slog"
	"sync"
)

// Buffer holds what is logged to its logger, at any level, until Replay logs it to
// another logger. What is logged before that logger is set up still follows its
// level, format and target.
type Buffer struct {
	logger *slog.Logger

	mu       sync.Mutex
	replayMu sync.Mutex
	entries  []bufferedRecord
}

type bufferedRecord struct {
	record slog.Record
	indent string
}

// NewBuffer returns an empty Buffer.
func NewBuffer() *Buffer {
	b := new(Buffer)
	b.logger = slog.New(&recordHandler{sink: b})

	return b
}

// Logger returns a logger writing to the buffer.
func (b *Buffer) Logger() *slog.Logger { return b.logger }

func (*Buffer) Enabled(context.Context, slog.Level) bool { return true }

func (b *Buffer) Handle(_ context.Context, record slog.Record, indent string) error {
	record = snapshotRecord(record)
	b.mu.Lock()
	defer b.mu.Unlock()

	b.entries = append(b.entries, bufferedRecord{record: record, indent: indent})

	return nil
}

// Replay logs the held entries to logger, as far as its level allows, and empties
// the buffer.
func (b *Buffer) Replay(logger *slog.Logger) {
	b.replayMu.Lock()
	defer b.replayMu.Unlock()

	b.mu.Lock()
	entries := b.entries
	b.entries = nil
	b.mu.Unlock()

	ctx := context.TODO()
	for _, entry := range entries {
		handler := logger.Handler()
		if !handler.Enabled(ctx, entry.record.Level) {
			continue
		}

		if h, ok := handler.(*recordHandler); ok && entry.indent != "" {
			handler = h.withIndent(entry.indent)
		}

		// Like slog.Logger, replay does not return handler errors. Managed output
		// handlers report failures to stderr themselves.
		_ = handler.Handle(ctx, entry.record.Clone())
	}
}
