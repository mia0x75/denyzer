package util

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/mia0x75/denyzer/log"
)

func TestFatalOnError(t *testing.T) {
	const helperEnv = "DENYZER_TEST_FATAL_ON_ERROR"
	if mode := os.Getenv(helperEnv); mode != "" {
		runFatalLoggingHelper(t, mode)

		return
	}

	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"text", "json", "stderr", "silenced", "discarded", "nil"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestFatalOnError$")
			// Ginkgo's coverage directory is a test flag, not inherited by
			// subprocesses. Avoid mixing os.Exit's coverage warning into JSON.
			cmd.Env = append(os.Environ(), helperEnv+"="+mode, "NO_COLOR=1", "GOCOVERDIR="+t.TempDir())
			output, err := cmd.CombinedOutput()
			if mode == "nil" {
				if err != nil || strings.Contains(string(output), "message test") {
					t.Fatalf("nil error logged or exited: %s, %v", output, err)
				}

				return
			}

			exitErr, ok := errors.AsType[*exec.ExitError](err)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("exit = %v, want status 1; output: %s", err, output)
			}

			if mode == "json" {
				var record map[string]any
				if err := json.Unmarshal(output, &record); err != nil {
					t.Fatalf("invalid JSON: %s: %v", output, err)
				}

				if record["level"] != "fatal" || record["msg"] != "message test" {
					t.Fatalf("fatal record = %#v", record)
				}
			} else if !strings.Contains(string(output), "FATAL message test") {
				t.Fatalf("missing fatal log: %s", output)
			}
		})
	}
}

func runFatalLoggingHelper(t *testing.T, mode string) {
	t.Helper()
	cfg := log.DefaultConfig()
	cfg.Timestamp = false
	if mode == "json" {
		cfg.Format = log.FormatTypeJson
	}

	if mode == "stderr" {
		cfg.Target = log.TargetTypeStderr
	}

	log.Configure(cfg)
	switch mode {
	case "silenced":
		log.Silence()
	case "discarded":
		log.SetOutput(log.Log(), io.Discard)
	case "nil":
		FatalOnError("message ", nil)

		return
	}

	FatalOnError("message ", errors.New("test"))
	t.Fatal("FatalOnError returned instead of exiting")
}
