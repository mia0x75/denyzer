package log

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Logger", func() {
	var cfg *Config

	BeforeEach(func() {
		cfg = DefaultConfig()
	})

	Describe("target", func() {
		It("defaults to stdout", func() {
			Expect(cfg.Target).Should(Equal(TargetTypeStdout))
		})

		It("parses each accepted value", func() {
			Expect(ParseTargetType("stdout")).Should(Equal(TargetTypeStdout))
			Expect(ParseTargetType("stderr")).Should(Equal(TargetTypeStderr))
			Expect(ParseTargetType("syslog")).Should(Equal(TargetTypeSyslog))
		})

		It("rejects anything else", func() {
			_, err := ParseTargetType("nowhere")
			Expect(err).Should(HaveOccurred())
		})
	})

	Describe("syslog", func() {
		It("defaults to the daemon facility and the denyzer tag", func() {
			Expect(cfg.Syslog.Facility).Should(Equal(SyslogFacility("daemon")))
			Expect(cfg.Syslog.Tag).Should(Equal("denyzer"))
		})

		It("keeps logging when syslog can't be used", func() {
			cfg.Target = TargetTypeSyslog
			cfg.Syslog.Facility = "not-a-facility"

			logger := NewLogger(cfg)

			out := new(bytes.Buffer)
			SetOutput(logger, out)
			logger.Warn("still here")

			Expect(out.String()).Should(ContainSubstring("still here"))
		})
	})
})

func TestLoggerInitialization(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	// Use a standalone importer without Unix syslog's incidental initialization dependencies.
	source, err := os.ReadFile("syslog_unsupported.go")
	if err != nil {
		t.Fatal(err)
	}
	_, body, found := bytes.Cut(source, []byte("package log"))
	if !found {
		t.Fatal("unsupported syslog source has no package declaration")
	}
	overlay, err := json.Marshal(map[string]map[string]string{
		"Replace": {
			filepath.Join(cwd, "syslog.go"):             "",
			filepath.Join(cwd, "syslog_unsupported.go"): filepath.Join(dir, "syslog.go"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"main.go": []byte(`package main
import "github.com/mia0x75/denyzer/log"
func main() { log.Log().Info("initialized") }
`),
		"syslog.go":    append([]byte("package log"), body...),
		"overlay.json": overlay,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(
		t.Context(),
		"go",
		"run",
		"-overlay="+filepath.Join(dir, "overlay.json"),
		filepath.Join(dir, "main.go"),
	)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("logger initialization: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("INFO initialized")) {
		t.Fatalf("global logger did not write default text output: %s", output)
	}
}
