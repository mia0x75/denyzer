package config

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/mia0x75/denyzer/log"
)

const (
	defaultHistoryMonths = 1
	maxHistoryMonths     = 12
)

// QueryLogRetention controls normal history cleanup for the embedded SQLite
// store. Cleanup removes query logs and short-lived metrics older than the
// selected cutoff while preserving all-time counters and global daily metrics.
type QueryLogRetention struct {
	// Delete query logs and short-lived metrics older than N months (1-12).
	// 0 → default 1.
	HistoryMonths int `default:"1" yaml:"historyMonths"`
}

// QueryLog configuration for the query logging
type QueryLog struct {
	// Path of the SQLite database file, or a directory that stores it as
	// denyzer.db.
	Target Secret `yaml:"target"`
	// Log target type: sqlite (default when omitted), console, or none.
	Type QueryLogType `yaml:"type"`
	// Per-table TTL retention settings for the embedded store.
	Retention QueryLogRetention `yaml:"retention"`
	// Maximum number of attempts to create the query log writer on startup.
	CreationAttempts int `default:"3" yaml:"creationAttempts"`
	// Delay between query log writer creation attempts.
	CreationCooldown Duration `default:"2s" yaml:"creationCooldown"`
	// Which fields to include in log entries; defaults to all available fields.
	Fields []QueryLogField `yaml:"fields"`
	// Interval at which buffered log entries are flushed in bulk to the database.
	FlushInterval Duration `default:"30s" yaml:"flushInterval"`
	// Maximum number of entries accumulated before a flush is triggered.
	BatchSize int `default:"100" yaml:"batchSize"`
	// Rules to suppress certain queries from being logged.
	Ignore QueryLogIgnore `yaml:"ignore"`
}

// EffectiveHistoryMonths returns the configured or default cleanup window,
// clamped to the supported 1-12 month range.
func (r QueryLogRetention) EffectiveHistoryMonths() int {
	months := r.HistoryMonths
	if months <= 0 {
		months = defaultHistoryMonths
	}

	return min(months, maxHistoryMonths)
}

type QueryLogIgnore struct {
	// If true, queries resolved as Special Use Domain Names (SUDN) are not logged.
	SUDN bool `default:"false" yaml:"sudn"`
	// Domains whose queries are not logged. Each entry is matched against the
	// query name as an exact domain, a *.wildcard, or a /regex/.
	Domains []string `yaml:"domains"`
}

// SetDefaults implements `defaults.Setter`.
func (c *QueryLog) SetDefaults() {
	// Since the default depends on the enum values, set it dynamically
	// to avoid having to repeat the values in the annotation.
	c.Fields = QueryLogFieldValues()
}

// IsEnabled implements `config.Configurable`.
func (c *QueryLog) IsEnabled() bool {
	return c.Type != QueryLogTypeNone
}

// LogConfig implements `config.Configurable`.
func (c *QueryLog) LogConfig(logger *slog.Logger) {
	logger.Info(fmt.Sprintf("type: %s", c.Type))

	if c.Target != "" {
		logger.Info("target: " + c.Target.Reveal())
	}

	logger.Info(fmt.Sprintf("retention: history=%dmo", c.Retention.EffectiveHistoryMonths()))
	if logger.Enabled(context.TODO(), slog.LevelDebug) {
		logger.Debug(fmt.Sprintf("creationAttempts: %d", c.CreationAttempts))
	}
	if logger.Enabled(context.TODO(), slog.LevelDebug) {
		logger.Debug(fmt.Sprintf("creationCooldown: %s", c.CreationCooldown))
	}
	logger.Info(fmt.Sprintf("flushInterval: %s", c.FlushInterval))
	logger.Info(fmt.Sprintf("batchSize: %d", c.BatchSize))
	logger.Info(fmt.Sprintf("fields: %s", c.Fields))

	logger.Info("ignore:")
	log.WithIndent(logger, "  ", func(e *slog.Logger) {
		e.Info(fmt.Sprintf("sudn: %t", c.Ignore.SUDN))

		if len(c.Ignore.Domains) > 0 {
			e.Info(fmt.Sprintf("domains (%d):", len(c.Ignore.Domains)))
			log.WithIndent(e, "  ", func(e *slog.Logger) {
				for _, d := range c.Ignore.Domains {
					if e.Enabled(context.TODO(), slog.LevelDebug) {
						e.Debug("- " + d)
					}
				}
			})
		}
	})
}
