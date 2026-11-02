package log

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
)

// syslogTransport accepts severity directly, not markers embedded in bytes.
type syslogTransport interface {
	writeRecord(level slog.Level, data []byte) error
	closeSyslog() error
}

type outputSink struct {
	mu        sync.Mutex
	out       io.Writer
	syslog    syslogTransport
	format    FormatType
	timestamp bool
	colors    bool
}

// Like the standard slog handlers, reuse rendering buffers across writes.
//
//nolint:gochecknoglobals
var outputBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func (*outputSink) Enabled(context.Context, slog.Level) bool { return true }

func (s *outputSink) Handle(_ context.Context, record slog.Record, indent string) error {
	return s.handleAttrs(record, recordAttrs(record), indent)
}

func (s *outputSink) handleAttrs(record slog.Record, attrs []slog.Attr, indent string) error {
	buffer := outputBuffers.Get().(*bytes.Buffer) //nolint:forcetypeassert // pool only holds buffers
	buffer.Reset()
	defer func() {
		// Do not keep unusually large messages alive in the pool.
		if buffer.Cap() <= 64*1024 {
			outputBuffers.Put(buffer)
		}
	}()

	if s.format == FormatTypeJson {
		if err := renderJSON(buffer, record, attrs); err != nil {
			return err
		}
	} else {
		renderText(buffer, record, attrs, indent, s.timestamp, s.colors, s.syslog != nil)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.syslog != nil {
		return s.syslog.writeRecord(record.Level, buffer.Bytes())
	}

	n, err := s.out.Write(buffer.Bytes())
	if err == nil && n != buffer.Len() {
		err = io.ErrShortWrite
	}

	return err
}

func (s *outputSink) close() error {
	if s.syslog != nil {
		return s.syslog.closeSyslog()
	}

	return nil
}

func jsonFields(attrs []slog.Attr) map[string]any {
	fields := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		if attr.Value.Kind() == slog.KindGroup {
			fields[attr.Key] = jsonFields(attr.Value.Group())
		} else if err, ok := attr.Value.Any().(error); ok {
			fields[attr.Key] = err.Error()
		} else {
			fields[attr.Key] = attr.Value.Any()
		}
	}

	return fields
}
