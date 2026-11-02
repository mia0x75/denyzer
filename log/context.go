package log

import (
	"context"
	"log/slog"
	"slices"
)

type ctxKey struct{}

// NewCtx stores an explicit logger, for scoped initialization or test capture.
// Request fields should use CtxWithAttrs instead, keeping loggers reusable.
func NewCtx(ctx context.Context, logger *slog.Logger) (context.Context, *slog.Logger) {
	ctx = context.WithValue(ctx, ctxKey{}, logger)

	return ctx, logger
}

// FromCtx returns the explicitly scoped logger, or the global logger.
func FromCtx(ctx context.Context) *slog.Logger {
	if logger, ok := LoggerInCtx(ctx); ok {
		return logger
	}

	return Log()
}

// LoggerInCtx returns only an explicitly scoped logger, without allocating.
func LoggerInCtx(ctx context.Context) (*slog.Logger, bool) {
	logger, ok := ctx.Value(ctxKey{}).(*slog.Logger)

	return logger, ok && logger != nil
}

// WrapCtx derives an immutable, explicitly scoped logger.
func WrapCtx(ctx context.Context, wrap func(*slog.Logger) *slog.Logger) (context.Context, *slog.Logger) {
	logger := wrap(FromCtx(ctx))

	return NewCtx(ctx, logger)
}

// CtxWithAttrs adds request-scoped fields. Handlers read them only for enabled
// records; disabled logging does not derive a logger or resolve deferred values.
func CtxWithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(attrsKey{}).([]slog.Attr)
	fields := make([]slog.Attr, 0, len(existing)+len(attrs))
	fields = append(fields, existing...)
	fields = append(fields, cloneAttrs(attrs)...)

	return context.WithValue(ctx, attrsKey{}, slices.Clip(fields))
}
