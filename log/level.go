package log

import (
	"fmt"
	"log/slog"
	"strings"
)

// Custom levels retain the distinctions accepted by historical configurations.
// Larger slog levels indicate more severe events.
const (
	LevelTrace slog.Level = slog.LevelDebug - 4
	LevelFatal slog.Level = slog.LevelError + 4
	LevelPanic slog.Level = slog.LevelError + 8
)

// Level is a slog level accepting the historical YAML level names.
type Level slog.Level

// Level implements slog.Leveler.
func (l Level) Level() slog.Level { return slog.Level(l) }

// String returns the historical, lowercase spelling of the level.
func (l Level) String() string { return levelName(l.Level()) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (l *Level) UnmarshalText(text []byte) error {
	var level slog.Level

	switch strings.ToLower(string(text)) {
	case "trace":
		level = LevelTrace
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	case "fatal":
		level = LevelFatal
	case "panic":
		level = LevelPanic
	default:
		return fmt.Errorf("invalid log level %q", text)
	}

	*l = Level(level)

	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

func levelName(level slog.Level) string {
	switch level {
	case LevelTrace:
		return "trace"
	case slog.LevelDebug:
		return "debug"
	case slog.LevelInfo:
		return "info"
	case slog.LevelWarn:
		return "warning"
	case slog.LevelError:
		return "error"
	case LevelFatal:
		return "fatal"
	case LevelPanic:
		return "panic"
	default:
		return strings.ToLower(level.String())
	}
}
