//go:build !windows && !plan9

package log

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// listen returns a UDP socket standing in for a syslog server, and the lines it receives.
func listen() (*net.UDPConn, string) {
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	Expect(err).Should(Succeed())

	conn, err := net.ListenUDP("udp", addr)
	Expect(err).Should(Succeed())

	return conn, conn.LocalAddr().String()
}

func receive(conn *net.UDPConn) string {
	Expect(conn.SetReadDeadline(time.Now().Add(5 * time.Second))).Should(Succeed())

	buf := make([]byte, 1024)

	n, _, err := conn.ReadFrom(buf)
	Expect(err).Should(Succeed())

	return string(buf[:n])
}

var _ = Describe("Syslog target", func() {
	var (
		conn   *net.UDPConn
		logger *slog.Logger
	)

	BeforeEach(func() {
		var address string

		conn, address = listen()
		DeferCleanup(conn.Close)

		cfg := DefaultConfig()
		cfg.Level = Level(LevelTrace)
		cfg.Target = TargetTypeSyslog
		cfg.Syslog.Network = "udp"
		cfg.Syslog.Address = address

		logger = NewLogger(cfg)
		DeferCleanup(func() { SetOutput(logger, io.Discard) })
	})

	It("maps every declared facility to a syslog priority", func() {
		for _, facility := range SyslogFacilityNames() {
			_, found := syslogFacilities[SyslogFacility(facility)]
			Expect(found).Should(BeTrue(), "facility %q must have a syslog priority", facility)
		}
	})

	// facility daemon (3) * 8 + severity
	DescribeTable("logs each level at the matching priority",
		func(level slog.Level, priority string) {
			logger.Log(context.Background(), level, "hello")

			Expect(receive(conn)).Should(HavePrefix(priority))
		},
		Entry("error is err", slog.LevelError, "<27>"),
		Entry("warn is warning", slog.LevelWarn, "<28>"),
		Entry("info is info", slog.LevelInfo, "<30>"),
		Entry("debug is debug", slog.LevelDebug, "<31>"),
		Entry("trace is debug", LevelTrace, "<31>"),
		Entry("fatal is crit", LevelFatal, "<26>"),
		Entry("panic is crit", LevelPanic, "<26>"),
	)

	It("carries the message and the tag", func() {
		logger.Info("a message")

		received := receive(conn)
		Expect(received).Should(ContainSubstring("denyzer"))
		Expect(received).Should(ContainSubstring("a message"))
	})

	It("leaves the level and the timestamp to syslog", func() {
		logger.Info("a message")

		received := receive(conn)
		Expect(received).ShouldNot(ContainSubstring("INFO"))
		// the record's own header is the only timestamp
		Expect(received).ShouldNot(MatchRegexp(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`))
	})

	It("keeps the prefix and any fields", func() {
		SetPrefix(logger, "resolver").With("upstream", "1.1.1.1").Warn("slow")

		Expect(receive(conn)).Should(ContainSubstring("resolver: slow upstream=1.1.1.1"))
	})

	It("applies indentation before formatting the syslog entry", func() {
		WithIndent(logger.With("prefix", "resolver"), "  ", func(entry *slog.Logger) {
			entry.Info("indented")
		})

		Expect(receive(conn)).Should(ContainSubstring("resolver:   indented"))
	})

	It("stops sending to syslog when reconfigured away from it", func() {
		ConfigureLogger(logger, DefaultConfig())

		Expect(managedSink(logger).output.syslog).Should(BeNil())

		out := new(bytes.Buffer)
		SetOutput(logger, out)
		logger.Info("stdout")
		Expect(out.String()).Should(ContainSubstring("stdout"))

		Expect(conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))).Should(Succeed())
		_, _, err := conn.ReadFrom(make([]byte, 1024))
		Expect(err).Should(HaveOccurred(), "reconfigured logger must not send to its old syslog destination")
	})

	It("keeps the recorders registered before it", func() {
		recorder, cleanup := Capture(logger)
		DeferCleanup(cleanup)

		cfg := DefaultConfig()
		cfg.Target = TargetTypeSyslog
		cfg.Syslog.Network = "udp"
		cfg.Syslog.Address = conn.LocalAddr().String()
		ConfigureLogger(logger, cfg)

		logger.Info("hooked")

		Expect(recorder.Messages()).Should(ContainElement("hooked"))
		Expect(receive(conn)).Should(ContainSubstring("hooked"))
	})

	DescribeTable("closes the syslog connection when reconfigured",
		func(next func() *Config) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			Expect(err).Should(Succeed())
			DeferCleanup(listener.Close)

			cfg := DefaultConfig()
			cfg.Target = TargetTypeSyslog
			cfg.Syslog.Network = "tcp"
			cfg.Syslog.Address = listener.Addr().String()
			ConfigureLogger(logger, cfg)

			client, err := listener.Accept()
			Expect(err).Should(Succeed())
			DeferCleanup(client.Close)

			ConfigureLogger(logger, next())

			Expect(client.SetReadDeadline(time.Now().Add(time.Second))).Should(Succeed())
			_, err = client.Read(make([]byte, 1))
			Expect(err).Should(Equal(io.EOF))
		},
		Entry("to a stream", DefaultConfig),
		Entry("to syslog again", func() *Config {
			cfg := DefaultConfig()
			cfg.Target = TargetTypeSyslog
			cfg.Syslog.Network = "udp"
			cfg.Syslog.Address = conn.LocalAddr().String()

			return cfg
		}),
	)

	It("sends a multi-line entry as one record per line", func() {
		logger.Info("first\nsecond")

		Expect(receive(conn)).Should(ContainSubstring("first"))
		Expect(receive(conn)).Should(ContainSubstring("second"))
	})

	It("writes through its own output, never a discarding one", func() {
		Expect(managedSink(logger).output.syslog).Should(BeAssignableToTypeOf(&syslogWriter{}))
		Expect(IsSilenced(logger)).Should(BeFalse())
	})

	When("syslog can't be used", func() {
		var cfg *Config

		BeforeEach(func() {
			cfg = DefaultConfig()
			cfg.Target = TargetTypeSyslog
		})

		It("rejects an address without a network", func() {
			cfg.Syslog.Address = "loghost:514"

			_, err := newSyslogWriter(cfg.Syslog)
			Expect(err).Should(MatchError(ContainSubstring("needs a network")))
		})

		It("falls back to stderr with the regular text format", func() {
			cfg.Syslog.Address = "loghost:514"

			ConfigureLogger(logger, cfg)

			Expect(managedSink(logger).output.syslog).Should(BeNil())
			Expect(managedSink(logger).output.format).Should(Equal(FormatTypeText))
		})
	})
})

func TestSyslogFallback(t *testing.T) {
	const helperEnv = "DENYZER_TEST_SYSLOG_FALLBACK"
	if mode := os.Getenv(helperEnv); mode != "" {
		runSyslogFallbackHelper(t, mode)

		return
	}
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"invalid facility", "address without network", "connection failure", "JSON fallback"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSyslogFallback$")
			cmd.Env = append(os.Environ(), helperEnv+"="+mode, "NO_COLOR=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("fallback helper: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), "still logging") {
				t.Fatalf("syslog fallback wrote to stdout instead of stderr: %s", stdout.String())
			}
			if !strings.Contains(stderr.String(), "can't log to syslog, using stderr instead:") ||
				!strings.Contains(stderr.String(), "still logging") {
				t.Fatalf("syslog fallback was not reported on stderr: %s", stderr.String())
			}
			if mode == "JSON fallback" {
				for line := range strings.SplitSeq(strings.TrimSpace(stderr.String()), "\n") {
					if !strings.HasPrefix(line, "{") || !strings.Contains(line, `"prefix":"resolver"`) {
						t.Fatalf("syslog fallback did not retain JSON formatting: %s", line)
					}
				}
			}
		})
	}
}

func runSyslogFallbackHelper(t *testing.T, mode string) {
	t.Helper()
	logger := NewLogger(DefaultConfig())
	SetOutput(logger, io.Discard)
	logger = SetPrefix(logger.With("static", "field"), "resolver")
	recorder, stop := Capture(logger)
	defer stop()
	cfg := DefaultConfig()
	cfg.Target = TargetTypeSyslog
	cfg.Timestamp = false
	switch mode {
	case "invalid facility", "JSON fallback":
		cfg.Syslog.Facility = "not-a-facility"
	case "address without network":
		cfg.Syslog.Address = "loghost:514"
	case "connection failure":
		cfg.Syslog.Network = "unixgram"
		cfg.Syslog.Address = filepath.Join(t.TempDir(), "missing.sock")
	}
	if mode == "JSON fallback" {
		cfg.Format = FormatTypeJson
	}
	ConfigureLogger(logger, cfg)
	logger.Info("still logging")
	records := recorder.Records()
	if len(records) != 2 || records[0].Level != slog.LevelError || records[1].Message != "still logging" {
		t.Fatal("syslog fallback lost its diagnostic or subsequent event")
	}
	for _, record := range records {
		fields := RecordFields(record)
		if fields["prefix"] != "resolver" || fields["static"] != "field" {
			t.Fatalf("fallback lost derived fields: %#v", fields)
		}
	}
}
