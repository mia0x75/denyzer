package server

import (
	"testing"

	"github.com/mia0x75/denyzer/log"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func init() {
	log.Silence()
}

func TestDNSServer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Server Suite")
}

// capturedLog is the handle returned by installLogRecorder.
type capturedLog struct {
	hook    *log.Recorder
	restore func()
}

// installLogRecorder attaches a recorder to the global logger so a
// test can assert on emitted log messages. The returned handle's
// uninstall() must be called (typically via DeferCleanup) to restore
// the previous capture state.
func installLogRecorder() *capturedLog {
	hook, restore := log.Capture(log.Log())

	return &capturedLog{
		hook:    hook,
		restore: restore,
	}
}

func (c *capturedLog) messages() []string { return c.hook.Messages() }
func (c *capturedLog) uninstall()         { c.restore() }
