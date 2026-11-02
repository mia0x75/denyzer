package log

import (
	"bytes"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

const (
	textTimestampFormat = "2006-01-02 15:04:05"
	colorReset          = "\x1b[0m"
	colorPrefix         = "\x1b[0;1;34m"
	colorTimestamp      = "\x1b[0;97m"
)

func renderText(
	buffer *bytes.Buffer,
	record slog.Record,
	attrs []slog.Attr,
	indent string,
	timestamp, colors, syslog bool,
) {
	var prefix string
	hasPrefix := false
	for _, attr := range attrs {
		if attr.Key == prefixField && attr.Value.Kind() == slog.KindString {
			prefix, hasPrefix = attr.Value.String(), true
		}
	}

	message := indent + record.Message
	parsePrefix := !syslog && !hasPrefix && strings.HasPrefix(message, "[")
	if parsePrefix {
		if end := strings.IndexByte(message, ']'); end > 1 {
			prefix, hasPrefix = message[1:end], true
			message = strings.TrimSpace(message[end+1:])
		}
	}

	if syslog {
		if prefix != "" {
			buffer.WriteString(prefix)
			buffer.WriteString(": ")
		}
	} else {
		if timestamp && !record.Time.IsZero() {
			writeColored(buffer, "["+record.Time.Format(textTimestampFormat)+"]", colorTimestamp, colors)
			buffer.WriteByte(' ')
		}

		writeTextLevel(buffer, record.Level, colors)
		if hasPrefix {
			writeColored(buffer, " "+prefix+":", colorPrefix, colors)
		}

		buffer.WriteByte(' ')
	}

	buffer.WriteString(message)
	writeTextAttrs(buffer, record.Level, attrs, colors, syslog)
	buffer.WriteByte('\n')
}

func writeTextAttrs(buffer *bytes.Buffer, level slog.Level, attrs []slog.Attr, colors, syslog bool) {
	fields := flattenAttrs(nil, "", attrs)
	slices.SortFunc(fields, func(a, b slog.Attr) int { return strings.Compare(a.Key, b.Key) })
	for _, attr := range fields {
		if attr.Key == prefixField {
			continue
		}

		buffer.WriteByte(' ')
		writeColored(buffer, attr.Key, levelColor(level), colors && !syslog)
		buffer.WriteByte('=')
		if syslog {
			fmt.Fprint(buffer, attr.Value.Any())
		} else {
			fmt.Fprintf(buffer, "%+v", attr.Value.Any())
		}
	}
}

func writeTextLevel(buffer *bytes.Buffer, level slog.Level, colors bool) {
	name := strings.ToUpper(levelName(level))
	if level == slog.LevelWarn {
		name = "WARN"
	}
	if len(name) < 5 {
		name = "     "[:5-len(name)] + name
	}

	writeColored(buffer, name, levelColor(level), colors)
}

func flattenAttrs(result []slog.Attr, prefix string, attrs []slog.Attr) []slog.Attr {
	for _, attr := range attrs {
		if attr.Value.Kind() == slog.KindGroup {
			result = flattenAttrs(result, prefix+attr.Key+".", attr.Value.Group())
		} else {
			attr.Key = prefix + attr.Key
			result = append(result, attr)
		}
	}

	return result
}

func levelColor(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "\x1b[0;31m"
	case level >= slog.LevelWarn:
		return "\x1b[0;33m"
	case level >= slog.LevelInfo:
		return "\x1b[0;32m"
	default:
		return "\x1b[0;34m"
	}
}

func writeColored(buffer *bytes.Buffer, value, color string, enabled bool) {
	if enabled && value != "" {
		buffer.WriteString(color)
		buffer.WriteString(value)
		buffer.WriteString(colorReset)
	} else {
		buffer.WriteString(value)
	}
}
