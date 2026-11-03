package cmd

import (
	"bytes"

	"github.com/mia0x75/denyzer/util"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Version command", func() {
	When("Version command is called", func() {
		It("should execute without error", func() {
			var output bytes.Buffer
			c := NewVersionCommand()
			c.SetOut(&output)
			c.SetArgs(make([]string, 0))
			err := c.Execute()
			Expect(err).Should(Succeed())
			Expect(output.String()).Should(Equal("Version: " + util.Version +
				"\nBuild time: " + util.BuildTime + "\nArchitecture: " + util.Architecture + "\n"))
		})
	})
})
