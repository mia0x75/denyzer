// Package log provides configurable slog handlers, request attributes, startup
// buffering and test capture while retaining Denyzer's logging output formats.
package log

//go:generate go tool go-enum -f=$GOFILE --marshal --names --template ../tools/schemagen/templates/enum_description.tmpl

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/creasty/defaults"
	"github.com/mattn/go-colorable"
	"golang.org/x/term"
)

const prefixField = "prefix"

// The global logger is stable: every derived logger shares its switching sink.
//
//nolint:gochecknoglobals
var logger *slog.Logger

//nolint:gochecknoinits // Reflection-based defaults need the generated enum maps initialized first.
func init() { logger = NewLogger(DefaultConfig()) }

// FormatType format for logging ENUM(
// text // Human-readable text.
// json // Structured JSON.
// )
type FormatType int

// TargetType destination for logging ENUM(
// stdout // Standard output.
// stderr // Standard error.
// syslog // System log, each entry at the priority matching its level.
// )
type TargetType int

// Config defines all logging configurations.
type Config struct {
	Level     Level        `default:"info"   yaml:"level"`
	Format    FormatType   `default:"text"   yaml:"format"`
	Target    TargetType   `default:"stdout" yaml:"target"`
	Syslog    SyslogConfig `yaml:"syslog"`
	Privacy   bool         `default:"false"  yaml:"privacy"`
	Timestamp bool         `default:"true"   yaml:"timestamp"`
}

// SyslogConfig defines where and as what denyzer logs when the target is syslog.
type SyslogConfig struct {
	// Network is empty for the local syslog socket, otherwise "udp" or "tcp".
	Network  string         `default:""        yaml:"network"`
	Address  string         `default:""        yaml:"address"`
	Tag      string         `default:"denyzer" yaml:"tag"`
	Facility SyslogFacility `default:"daemon"  yaml:"facility"`
}

// SyslogFacility is the facility used for syslog messages. ENUM(
// daemon // System daemon messages.
// user // User-level messages.
// local0 // Local use facility 0.
// local1 // Local use facility 1.
// local2 // Local use facility 2.
// local3 // Local use facility 3.
// local4 // Local use facility 4.
// local5 // Local use facility 5.
// local6 // Local use facility 6.
// local7 // Local use facility 7.
// )
type SyslogFacility string

// DefaultConfig returns a new Config initialized with default values.
func DefaultConfig() *Config {
	cfg := new(Config)
	defaults.MustSet(cfg)

	return cfg
}

// NewLogger returns an independently configurable logger.
func NewLogger(cfg *Config) *slog.Logger {
	result := slog.New(&recordHandler{sink: new(switchSink)})
	ConfigureLogger(result, cfg)

	return result
}

// Log returns the global logger.
func Log() *slog.Logger { return logger }

// PrefixedLog returns the global logger with prefix.
func PrefixedLog(prefix string) *slog.Logger { return SetPrefix(logger, prefix) }

// SetPrefix replaces the logger's resolver prefix without accumulating earlier
// resolvers' prefixes. Request-scoped fields remain in the logging context.
func SetPrefix(logger *slog.Logger, prefix string) *slog.Logger {
	if handler, ok := logger.Handler().(*recordHandler); ok {
		attrs := handler.attrs
		// A prefix describes the logger, not its current attribute group.
		attrs.scopes = appendScope(attrs.scopes, attrScope{attrs: []slog.Attr{slog.String(prefixField, prefix)}})

		return slog.New(&recordHandler{sink: handler.sink, attrs: attrs})
	}

	return logger.With(prefixField, prefix)
}

// WithPrefix appends a dotted suffix, such as blocking.client_id_cache.
func WithPrefix(logger *slog.Logger, prefix string) *slog.Logger {
	if handler, ok := logger.Handler().(*recordHandler); ok {
		if existing, found := handler.attrs.prefix(); found {
			prefix = existing + "." + prefix
		}
	}

	return SetPrefix(logger, prefix)
}

// WithAttrs derives a logger from typed attributes.
func WithAttrs(logger *slog.Logger, attrs ...slog.Attr) *slog.Logger {
	return slog.New(logger.Handler().WithAttrs(attrs))
}

// WithIndent indents this callback's text messages, without modifying shared
// loggers or changing JSON messages. Nested indentation is additive.
func WithIndent(logger *slog.Logger, indent string, callback func(*slog.Logger)) {
	if handler, ok := logger.Handler().(*recordHandler); ok {
		logger = slog.New(handler.withIndent(indent))
	}

	callback(logger)
}

// EscapeInput removes line breaks from input.
func EscapeInput(input string) string {
	result := strings.ReplaceAll(input, "\n", "")

	return strings.ReplaceAll(result, "\r", "")
}

// Configure applies configuration to the global logger.
func Configure(cfg *Config) { ConfigureLogger(logger, cfg) }

// ConfigureLogger configures a logger created by NewLogger (or derived from it).
// A syslog setup failure falls back to stderr and logs the failure there.
func ConfigureLogger(logger *slog.Logger, cfg *Config) {
	sink := managedSink(logger)
	output, err := configuredOutput(cfg)
	sink.mu.Lock()
	sink.cfg = *cfg
	sink.replaceLocked(output, cfg.Level.Level())
	sink.mu.Unlock()
	if err != nil {
		logger.Error("can't log to syslog, using stderr instead: " + err.Error())
	}
}

func configuredOutput(cfg *Config) (*outputSink, error) {
	if cfg.Target != TargetTypeSyslog {
		return streamOutput(cfg, newOutput(cfg.Target)), nil
	}

	transport, err := newSyslogWriter(cfg.Syslog)
	if err != nil {
		return streamOutput(cfg, newOutput(TargetTypeStderr)), err
	}

	return &outputSink{syslog: transport, format: cfg.Format}, nil
}

func streamOutput(cfg *Config, out io.Writer) *outputSink {
	colors := false
	if file, ok := out.(*os.File); ok && !noColor() {
		colors = term.IsTerminal(int(file.Fd()))
	}

	return &outputSink{out: out, format: cfg.Format, timestamp: cfg.Timestamp, colors: colors}
}

func newOutput(target TargetType) io.Writer {
	if target == TargetTypeStderr {
		if noColor() {
			return os.Stderr
		}

		return colorable.NewColorableStderr()
	}

	if noColor() {
		return os.Stdout
	}

	// Windows does not support ANSI colors natively.
	return colorable.NewColorableStdout()
}

// Respect NO_COLOR (https://no-color.org/).
func noColor() bool { return os.Getenv("NO_COLOR") != "" }

// Silence disables the global output, but keeps the global logger identity.
// Recorders installed afterwards can still capture enabled events.
func Silence() {
	sink := managedSink(logger)
	sink.mu.Lock()
	sink.cfg = *DefaultConfig()
	sink.recorders = nil
	sink.replaceLocked(nil, slog.LevelInfo)
	sink.mu.Unlock()
}
