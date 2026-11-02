//go:build !windows && !plan9

package log

import (
	"fmt"
	"log/slog"
	"log/syslog"
	"slices"
	"strings"
)

// Facilities a DNS server can sensibly claim: the system daemon facility and the
// eight reserved for local use.
//
//nolint:gochecknoglobals
var syslogFacilities = map[SyslogFacility]syslog.Priority{
	SyslogFacilityDaemon: syslog.LOG_DAEMON,
	SyslogFacilityUser:   syslog.LOG_USER,
	SyslogFacilityLocal0: syslog.LOG_LOCAL0,
	SyslogFacilityLocal1: syslog.LOG_LOCAL1,
	SyslogFacilityLocal2: syslog.LOG_LOCAL2,
	SyslogFacilityLocal3: syslog.LOG_LOCAL3,
	SyslogFacilityLocal4: syslog.LOG_LOCAL4,
	SyslogFacilityLocal5: syslog.LOG_LOCAL5,
	SyslogFacilityLocal6: syslog.LOG_LOCAL6,
	SyslogFacilityLocal7: syslog.LOG_LOCAL7,
}

// newSyslogWriter connects to syslog, accepting each record's severity directly.
func newSyslogWriter(cfg SyslogConfig) (syslogTransport, error) {
	facility, found := syslogFacilities[cfg.Facility]
	if !found {
		names := SyslogFacilityNames()
		slices.Sort(names)

		return nil, fmt.Errorf("invalid syslog facility '%s', try one of: %s",
			cfg.Facility, strings.Join(names, ", "))
	}

	// syslog.Dial ignores the address without a network and logs locally instead
	if cfg.Network == "" && cfg.Address != "" {
		return nil, fmt.Errorf("syslog address '%s' needs a network, set it to udp or tcp", cfg.Address)
	}

	// an empty network dials the local syslog socket
	writer, err := syslog.Dial(cfg.Network, cfg.Address, facility, cfg.Tag)
	if err != nil {
		return nil, fmt.Errorf("can't connect to syslog: %w", err)
	}

	return &syslogWriter{writer: writer}, nil
}

type syslogWriter struct {
	writer *syslog.Writer
}

func (w *syslogWriter) closeSyslog() error {
	return w.writer.Close()
}

func (w *syslogWriter) writeRecord(level slog.Level, line []byte) error {
	write := w.writeFunc(level)

	// a syslog record is a single line, so a multi-line entry becomes one record
	// per line rather than one record containing newlines
	for message := range strings.SplitSeq(strings.TrimRight(string(line), "\n"), "\n") {
		if err := write(message); err != nil {
			return err
		}
	}

	return nil
}

func (w *syslogWriter) writeFunc(level slog.Level) func(string) error {
	switch {
	case level >= LevelFatal:
		return w.writer.Crit
	case level >= slog.LevelError:
		return w.writer.Err
	case level >= slog.LevelWarn:
		return w.writer.Warning
	case level >= slog.LevelInfo:
		return w.writer.Info
	default:
		return w.writer.Debug
	}
}
