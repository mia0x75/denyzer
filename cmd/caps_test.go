package cmd

import (
	"errors"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("warnMissingPrivilegedPortCapability", func() {
	var (
		loggerHook *log.Recorder
		origRaise  func() (bool, error)
	)

	BeforeEach(func() {
		origRaise = raiseNetBindService
		var cleanup func()
		loggerHook, cleanup = log.Capture(log.Log())
		DeferCleanup(cleanup)
	})

	AfterEach(func() {
		raiseNetBindService = origRaise
		loggerHook.Reset()
	})

	When("the capability is effective", func() {
		BeforeEach(func() {
			raiseNetBindService = func() (bool, error) { return true, nil }
		})

		It("does not warn even with a privileged port", func() {
			warnMissingPrivilegedPortCapability(config.Ports{DNS: config.ListenConfig{":53"}})
			Expect(loggerHook.Records()).Should(BeEmpty())
		})
	})

	When("the capability is not effective", func() {
		BeforeEach(func() {
			raiseNetBindService = func() (bool, error) { return false, nil }
		})

		It("warns when a privileged port is configured", func() {
			warnMissingPrivilegedPortCapability(config.Ports{DNS: config.ListenConfig{":53"}})
			Expect(loggerHook.LastRecord().Message).Should(ContainSubstring("CAP_NET_BIND_SERVICE"))
		})

		It("does not warn when only unprivileged ports are configured", func() {
			warnMissingPrivilegedPortCapability(config.Ports{
				DNS:  config.ListenConfig{":1053"},
				HTTP: config.ListenConfig{":4000"},
			})
			Expect(loggerHook.Records()).Should(BeEmpty())
		})
	})

	When("raising the capability returns an error", func() {
		BeforeEach(func() {
			raiseNetBindService = func() (bool, error) { return false, errors.New("boom") }
		})

		It("logs one combined warning naming the error and the capability", func() {
			warnMissingPrivilegedPortCapability(config.Ports{DNS: config.ListenConfig{":53"}})
			Expect(loggerHook.Records()).Should(HaveLen(1))
			Expect(loggerHook.LastRecord().Message).Should(SatisfyAll(
				ContainSubstring("could not adjust process capabilities"),
				ContainSubstring("CAP_NET_BIND_SERVICE"),
			))
		})

		It("does not warn when no privileged port is configured", func() {
			warnMissingPrivilegedPortCapability(config.Ports{DNS: config.ListenConfig{":1053"}})
			Expect(loggerHook.Records()).Should(BeEmpty())
		})

		It("does not raise the capability when no privileged port is configured", func() {
			raised := false
			raiseNetBindService = func() (bool, error) {
				raised = true

				return false, nil
			}

			warnMissingPrivilegedPortCapability(config.Ports{DNS: config.ListenConfig{":1053"}})
			Expect(raised).Should(BeFalse())
			Expect(loggerHook.Records()).Should(BeEmpty())
		})
	})
})
