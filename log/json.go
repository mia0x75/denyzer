package log

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const jsonDecimalBase = 10

// Render typed attributes directly: converting each field to any and building a
// map makes reflection and boxing dominate enabled JSON logging. attrs and their
// groups are normalized, owned scratch slices, not a logger's bound attributes.
func renderJSON(buffer *bytes.Buffer, record slog.Record, attrs []slog.Attr) error {
	attrs = reserveJSONFields(attrs)
	// Historically JSON always contains time, irrespective of timestamp:false.
	if !record.Time.IsZero() {
		attrs = append(attrs, slog.String("time", record.Time.Format(time.RFC3339)))
	}
	attrs = append(attrs, slog.String("level", levelName(record.Level)), slog.String("msg", record.Message))
	if err := writeJSONAttrs(buffer, attrs); err != nil {
		return fmt.Errorf("encode log record: %w", err)
	}
	buffer.WriteByte('\n')

	return nil
}

func reserveJSONFields(attrs []slog.Attr) []slog.Attr {
	for _, key := range []string{"time", "level", "msg"} {
		index := slices.IndexFunc(attrs, func(attr slog.Attr) bool { return attr.Key == key })
		if index < 0 {
			continue
		}
		attr := attrs[index]
		attr.Key = "fields." + key
		attrs = slices.Delete(attrs, index, index+1)
		index = slices.IndexFunc(attrs, func(existing slog.Attr) bool { return existing.Key == attr.Key })
		if index >= 0 {
			attrs[index] = attr

			continue
		}
		attrs = append(attrs, attr)
	}

	return attrs
}

func writeJSONAttrs(buffer *bytes.Buffer, attrs []slog.Attr) error {
	slices.SortFunc(attrs, func(a, b slog.Attr) int { return strings.Compare(a.Key, b.Key) })
	buffer.WriteByte('{')
	for i, attr := range attrs {
		if i > 0 {
			buffer.WriteByte(',')
		}
		writeJSONString(buffer, attr.Key)
		buffer.WriteByte(':')
		if err := writeJSONValue(buffer, attr.Value); err != nil {
			return err
		}
	}
	buffer.WriteByte('}')

	return nil
}

func writeJSONValue(buffer *bytes.Buffer, value slog.Value) error {
	switch value.Kind() {
	case slog.KindString:
		writeJSONString(buffer, value.String())
	case slog.KindBool:
		buffer.Write(strconv.AppendBool(buffer.AvailableBuffer(), value.Bool()))
	case slog.KindInt64:
		buffer.Write(strconv.AppendInt(buffer.AvailableBuffer(), value.Int64(), jsonDecimalBase))
	case slog.KindUint64:
		buffer.Write(strconv.AppendUint(buffer.AvailableBuffer(), value.Uint64(), jsonDecimalBase))
	case slog.KindDuration:
		buffer.Write(strconv.AppendInt(buffer.AvailableBuffer(), int64(value.Duration()), jsonDecimalBase))
	case slog.KindGroup:
		return writeJSONAttrs(buffer, value.Group())
	case slog.KindAny, slog.KindFloat64, slog.KindTime, slog.KindLogValuer:
		// Preserve encoding/json's float, time, Marshaler, collection and error
		// semantics for values outside the common primitive logging fields.
		data, err := json.Marshal(value.Any())
		if err != nil {
			return err
		}
		buffer.Write(data)
	}

	return nil
}

func writeJSONString(buffer *bytes.Buffer, value string) {
	if !plainJSONString(value) {
		// Marshaling a string cannot fail. Keep escaping (including HTML,
		// invalid UTF-8 and Unicode separators) identical to encoding/json.
		data, _ := json.Marshal(value) //nolint:errchkjson // strings cannot fail to marshal
		buffer.Write(data)

		return
	}
	buffer.WriteByte('"')
	buffer.WriteString(value)
	buffer.WriteByte('"')
}

func plainJSONString(value string) bool {
	for i := range len(value) {
		character := value[i]
		if character < 0x20 || character >= utf8.RuneSelf {
			return false
		}
		switch character {
		case '"', '\\', '<', '>', '&':
			return false
		}
	}

	return true
}
