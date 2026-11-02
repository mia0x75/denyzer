package log

import (
	"context"
	"log/slog"
	"slices"
)

type attrsKey struct{}

type attrScope struct {
	groups []string
	attrs  []slog.Attr
}

// handlerAttrs is immutable after derivation. No request context or output is
// captured here, so prefixed loggers can be reused across requests and settings.
type handlerAttrs struct {
	scopes []attrScope
	groups []string
	indent string
}

func (a handlerAttrs) withAttrs(attrs []slog.Attr) handlerAttrs {
	a.scopes = appendScope(a.scopes, attrScope{groups: a.groups, attrs: cloneAttrs(attrs)})

	return a
}

func (a handlerAttrs) withGroup(name string) handlerAttrs {
	a.groups = append(slices.Clip(a.groups), name)

	return a
}

func appendScope(scopes []attrScope, scope attrScope) []attrScope {
	return append(slices.Clip(scopes), scope)
}

func (a handlerAttrs) prefix() (string, bool) {
	for _, scope := range slices.Backward(a.scopes) {
		if len(scope.groups) != 0 {
			continue
		}

		for _, attr := range slices.Backward(scope.attrs) {
			if attr.Key == prefixField && attr.Value.Kind() == slog.KindString {
				return attr.Value.String(), true
			}
		}
	}

	return "", false
}

func (a handlerAttrs) record(ctx context.Context, record slog.Record) slog.Record {
	attrs, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	if len(a.scopes) == 0 && len(a.groups) == 0 && len(attrs) == 0 {
		return record
	}

	result := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	result.AddAttrs(attrs...)

	for _, scope := range a.scopes {
		result.AddAttrs(groupAttrs(scope.groups, scope.attrs)...)
	}

	if len(a.groups) == 0 {
		record.Attrs(func(attr slog.Attr) bool {
			result.AddAttrs(attr)

			return true
		})
	} else {
		attrs := make([]slog.Attr, 0, record.NumAttrs())
		record.Attrs(func(attr slog.Attr) bool {
			attrs = append(attrs, attr)

			return true
		})
		result.AddAttrs(groupAttrs(a.groups, attrs)...)
	}

	return result
}

func groupAttrs(groups []string, attrs []slog.Attr) []slog.Attr {
	for _, group := range slices.Backward(groups) {
		attrs = []slog.Attr{{Key: group, Value: slog.GroupValue(attrs...)}}
	}

	return attrs
}

// cloneAttrs owns group backing slices without resolving deferred values.
func cloneAttrs(attrs []slog.Attr) []slog.Attr {
	result := slices.Clone(attrs)
	for i, attr := range result {
		if attr.Value.Kind() == slog.KindGroup {
			result[i].Value = slog.GroupValue(cloneAttrs(attr.Value.Group())...)
		}
	}

	return result
}
