package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/mia0x75/denyzer/log"
	"github.com/spf13/cobra"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func init() {
	log.Silence()
}

func TestCmd(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Command Suite")
}

var _ = BeforeEach(keepGlobals)

var _ = Describe("Command test isolation", Ordered, func() {
	var originalPath string

	BeforeAll(func() {
		originalPath = configPath
		Expect(log.IsSilenced(log.Log())).Should(BeTrue())
	})

	It("can change global command and logging settings", func() {
		configPath = "test-only-config.yml"
		log.SetOutput(log.Log(), new(bytes.Buffer))
		Expect(log.IsSilenced(log.Log())).Should(BeFalse())
	})

	It("restores settings before the next spec", func() {
		Expect(configPath).Should(Equal(originalPath))
		Expect(log.IsSilenced(log.Log())).Should(BeTrue())
	})
})

// withContext wraps a cobra.Command and sets a context on it for testing
func withContext(cmd *cobra.Command) *cobra.Command {
	ctx := context.Background()
	cmd.SetContext(ctx)

	return cmd
}

// keepGlobals sets the global logger and the command settings back as they are now once
// the spec ends
func keepGlobals() {
	restoreLog := log.SaveSettings(log.Log())
	path, host, port, dHost, dPort := configPath, apiHost, apiPort, dnsHost, dnsPort

	DeferCleanup(func() {
		restoreLog()

		configPath, apiHost, apiPort, dnsHost, dnsPort = path, host, port, dHost, dPort
	})
}
