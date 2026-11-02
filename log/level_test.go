package log

import (
	"log/slog"
	"testing"

	"gopkg.in/yaml.v2"
)

func TestLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		level slog.Level
		text  string
	}{
		{name: "trace", level: LevelTrace, text: "trace"},
		{name: "debug", level: slog.LevelDebug, text: "debug"},
		{name: "info", level: slog.LevelInfo, text: "info"},
		{name: "warn", level: slog.LevelWarn, text: "warning"},
		{name: "warning", level: slog.LevelWarn, text: "warning"},
		{name: "error", level: slog.LevelError, text: "error"},
		{name: "fatal", level: LevelFatal, text: "fatal"},
		{name: "panic", level: LevelPanic, text: "panic"},
		{name: "DEBUG", level: slog.LevelDebug, text: "debug"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var cfg struct {
				Level Level `yaml:"level"`
			}

			if err := yaml.UnmarshalStrict([]byte("level: "+tt.name), &cfg); err != nil {
				t.Fatal(err)
			}

			if cfg.Level.Level() != tt.level || cfg.Level.String() != tt.text {
				t.Fatalf("level = %s/%d, want %s/%d", cfg.Level, cfg.Level.Level(), tt.text, tt.level)
			}

			text, err := cfg.Level.MarshalText()
			if err != nil || string(text) != tt.text {
				t.Fatalf("MarshalText = %q, %v", text, err)
			}
		})
	}

	t.Run("rejects unknown values without changing the level", func(t *testing.T) {
		level := Level(slog.LevelWarn)
		if err := level.UnmarshalText([]byte("verbose")); err == nil {
			t.Fatal("accepted an unknown level")
		}

		if level.Level() != slog.LevelWarn {
			t.Fatal("failed parsing changed the level")
		}
	})
}
