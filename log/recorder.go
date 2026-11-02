package log

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
)

// Recorder captures records without producing output. Its snapshots and message
// lists are safe to inspect while other goroutines are logging.
type Recorder struct {
	level   slog.LevelVar
	mu      sync.Mutex
	records []slog.Record
}

// NewRecorder returns a recorder accepting every historical log level.
func NewRecorder() *Recorder {
	r := new(Recorder)
	r.level.Set(LevelTrace)

	return r
}

// Logger returns a logger writing to this recorder.
func (r *Recorder) Logger() *slog.Logger {
	return slog.New(&recordHandler{sink: r})
}

// NewMockLogger returns a logger and its concurrency-safe test recorder.
func NewMockLogger() (*slog.Logger, *Recorder) {
	r := NewRecorder()

	return r.Logger(), r
}

// SetLevel changes the minimum level captured by the recorder.
func (r *Recorder) SetLevel(level slog.Level) { r.level.Set(level) }

func (r *Recorder) Enabled(_ context.Context, level slog.Level) bool {
	return level >= r.level.Level()
}

func (r *Recorder) Handle(ctx context.Context, record slog.Record, _ string) error {
	if !r.Enabled(ctx, record.Level) {
		return nil
	}

	r.capture(record, recordAttrs(record))

	return nil
}

func (r *Recorder) capture(record slog.Record, attrs []slog.Attr) {
	record = snapshotRecordAttrs(record, attrs)
	r.mu.Lock()
	r.records = append(r.records, record)
	r.mu.Unlock()
}

// Records returns independent snapshots, not the recorder's backing slice.
func (r *Recorder) Records() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]slog.Record, len(r.records))
	for i, record := range r.records {
		result[i] = snapshotRecord(record)
	}

	return result
}

// LastRecord returns the last captured record, or a zero record when empty.
func (r *Recorder) LastRecord() slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.records) == 0 {
		return slog.Record{}
	}

	return snapshotRecord(r.records[len(r.records)-1])
}

// Messages returns a copy of the captured messages in emission order.
func (r *Recorder) Messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make([]string, len(r.records))
	for i, record := range r.records {
		result[i] = record.Message
	}

	return result
}

// Reset discards all captured records.
func (r *Recorder) Reset() {
	r.mu.Lock()
	r.records = nil
	r.mu.Unlock()
}

// RecordFields returns a record's resolved attributes, including nested groups.
func RecordFields(record slog.Record) map[string]any { return jsonFields(recordAttrs(record)) }

// Record.Clone alone is shallow: resolve lazy values and own groups, maps and
// slices (also inside arrays) before retaining records. Other Any values must be
// immutable, as with the standard slog handlers; this is not a pointer/struct
// deep-copy API.
func snapshotRecord(record slog.Record) slog.Record {
	return snapshotRecordAttrs(record, recordAttrs(record))
}

func snapshotRecordAttrs(record slog.Record, attrs []slog.Attr) slog.Record {
	result := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	for _, attr := range attrs {
		result.AddAttrs(snapshotAttr(attr))
	}

	return result.Clone()
}

func snapshotAttr(attr slog.Attr) slog.Attr {
	if attr.Value.Kind() == slog.KindGroup {
		children := cloneAttrs(attr.Value.Group())
		for i, child := range children {
			children[i] = snapshotAttr(child)
		}

		attr.Value = slog.GroupValue(children...)
	} else if attr.Value.Kind() == slog.KindAny {
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.StringValue(err.Error())
		} else if value := reflect.ValueOf(attr.Value.Any()); value.IsValid() {
			attr.Value = slog.AnyValue(cloneMutable(value, make(map[mutableKey]reflect.Value)).Interface())
		}
	}

	return attr
}

type mutableKey struct {
	typeOf  reflect.Type
	pointer uintptr
	length  int
}

func cloneMutable(value reflect.Value, seen map[mutableKey]reflect.Value) reflect.Value {
	//nolint:exhaustive // only collections are copied; other Any values are immutable by contract
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return value
		}

		result := reflect.New(value.Type()).Elem()
		result.Set(cloneMutable(value.Elem(), seen))

		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for i := range value.Len() {
			result.Index(i).Set(cloneMutable(value.Index(i), seen))
		}

		return result
	case reflect.Map, reflect.Slice:
		if value.IsNil() {
			return value
		}

		key := mutableKey{typeOf: value.Type(), pointer: value.Pointer(), length: value.Len()}
		if result, found := seen[key]; found {
			return result
		}

		if value.Kind() == reflect.Map {
			result := reflect.MakeMapWithSize(value.Type(), value.Len())
			seen[key] = result
			iter := value.MapRange()
			for iter.Next() {
				result.SetMapIndex(iter.Key(), cloneMutable(iter.Value(), seen))
			}

			return result
		}

		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		seen[key] = result
		for i := range value.Len() {
			result.Index(i).Set(cloneMutable(value.Index(i), seen))
		}

		return result
	default:
		return value
	}
}
