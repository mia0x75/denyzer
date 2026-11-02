package querylog

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/mia0x75/denyzer/log"
)

const loggerPrefixLoggerWriter = "queryLog"

type LoggerWriter struct {
	logger *slog.Logger
}

func NewLoggerWriter() *LoggerWriter {
	return &LoggerWriter{logger: log.PrefixedLog(loggerPrefixLoggerWriter)}
}

func (d *LoggerWriter) Write(entry *LogEntry) {
	fields := LogEntryFields(entry)

	d.logger.LogAttrs(context.TODO(), slog.LevelInfo, "query resolved", fields...)
}

func (d *LoggerWriter) RecordError() {
	// Nothing to do
}

func (d *LoggerWriter) CleanUp() {
	// Nothing to do
}

func LogEntryFields(entry *LogEntry) []slog.Attr {
	return withoutZeroes([]slog.Attr{
		slog.String("client_ip", entry.ClientIP),
		slog.String("client_names", strings.Join(entry.ClientNames, "; ")),
		slog.String("response_reason", entry.ResponseReason),
		slog.String("response_type", entry.ResponseType),
		slog.String("response_code", entry.ResponseCode),
		slog.String("question_name", entry.QuestionName),
		slog.String("question_type", entry.QuestionType),
		slog.String("answer", entry.Answer),
		slog.Int64("duration_ms", entry.DurationMs),
		slog.String("instance", entry.DenyzerInstance),
	})
}

func withoutZeroes(fields []slog.Attr) []slog.Attr {
	return slices.DeleteFunc(fields, func(attr slog.Attr) bool {
		return attr.Value.Kind() == slog.KindString && attr.Value.String() == "" ||
			attr.Value.Kind() == slog.KindInt64 && attr.Value.Int64() == 0
	})
}
