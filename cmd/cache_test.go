package cmd

import (
	"net/http"
	"net/http/httptest"

	"github.com/mia0x75/denyzer/log"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Cache command", func() {
	var (
		ts         *httptest.Server
		mockFn     func(w http.ResponseWriter, _ *http.Request)
		loggerHook *log.Recorder
	)
	JustBeforeEach(func() {
		ts = testHTTPAPIServer(mockFn)
	})
	JustAfterEach(func() {
		ts.Close()
	})
	BeforeEach(func() {
		mockFn = func(w http.ResponseWriter, _ *http.Request) {}
		var cleanup func()
		loggerHook, cleanup = log.Capture(log.Log())
		DeferCleanup(cleanup)
	})
	AfterEach(func() {
		loggerHook.Reset()
	})
	Describe("flush cache", func() {
		When("flush cache is called via REST", func() {
			It("should flush caches", func() {
				Expect(flushCache(withContext(newCacheCommand()), []string{})).Should(Succeed())
				Expect(loggerHook.LastRecord().Message).Should(Equal("OK"))
			})
		})
		When("Wrong url is used", func() {
			It("Should end with error", func() {
				apiPort = 0
				err := flushCache(withContext(newCacheCommand()), []string{})
				Expect(err).Should(HaveOccurred())
				Expect(err.Error()).Should(ContainSubstring("connection refused"))
			})
		})
	})
})
