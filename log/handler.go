package log

import (
	"context"
	"log/slog"
)

// recordSink receives fully scoped attributes and formatting-only indentation.
// Keeping derivation outside the sink lets existing loggers follow sink changes.
type recordSink interface {
	Enabled(ctx context.Context, level slog.Level) bool
	Handle(ctx context.Context, record slog.Record, indent string) error
}

type recordHandler struct {
	sink  recordSink
	attrs handlerAttrs
}

func (h *recordHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.sink.Enabled(ctx, level)
}

func (h *recordHandler) Handle(ctx context.Context, record slog.Record) error {
	return h.sink.Handle(ctx, h.attrs.record(ctx, record), h.attrs.indent)
}

func (h *recordHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordHandler{sink: h.sink, attrs: h.attrs.withAttrs(attrs)}
}

func (h *recordHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	return &recordHandler{sink: h.sink, attrs: h.attrs.withGroup(name)}
}

func (h *recordHandler) withIndent(indent string) slog.Handler {
	attrs := h.attrs
	attrs.indent += indent

	return &recordHandler{sink: h.sink, attrs: attrs}
}

func recordAttrs(record slog.Record) []slog.Attr {
	attrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		attrs = appendNormalizedAttr(attrs, attr)

		return true
	})

	return attrs
}

// Resolve only when a record is emitted. Merge repeated groups and replace
// repeated keys, matching the old field-map semantics without a per-call map.
func appendNormalizedAttr(attrs []slog.Attr, attr slog.Attr) []slog.Attr {
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindAny {
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.StringValue(err.Error())
		}
	}
	if attr.Equal(slog.Attr{}) {
		return attrs
	}

	if attr.Value.Kind() == slog.KindGroup {
		children := make([]slog.Attr, 0, len(attr.Value.Group()))
		for _, child := range attr.Value.Group() {
			children = appendNormalizedAttr(children, child)
		}

		if attr.Key == "" {
			for _, child := range children {
				attrs = appendNormalizedAttr(attrs, child)
			}

			return attrs
		}

		if len(children) == 0 {
			return attrs
		}

		attr.Value = slog.GroupValue(children...)
	}

	for i, existing := range attrs {
		if existing.Key != attr.Key {
			continue
		}

		if existing.Value.Kind() == slog.KindGroup && attr.Value.Kind() == slog.KindGroup {
			children := cloneAttrs(existing.Value.Group())
			for _, child := range attr.Value.Group() {
				children = appendNormalizedAttr(children, child)
			}

			attr.Value = slog.GroupValue(children...)
		}

		attrs[i] = attr

		return attrs
	}

	return append(attrs, attr)
}
