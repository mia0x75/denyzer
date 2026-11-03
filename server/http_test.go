package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"

	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/log"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HTTP middleware", func() {
	var handler http.Handler

	BeforeEach(func() {
		handler = withCommonMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	})

	It("routes HTTP server errors through the configured logger", func() {
		capture := installLogRecorder()
		DeferCleanup(capture.uninstall)
		server := newHTTPServer("admin", handler, &config.Config{})
		server.inner.ErrorLog.Print("http: TLS handshake error from 127.0.0.1:45122: unexpected EOF")
		record := capture.hook.LastRecord()
		Expect(record.Level).Should(Equal(slog.LevelWarn))
		Expect(record.Message).Should(ContainSubstring("unexpected EOF"))
		Expect(log.RecordFields(record)).Should(HaveKeyWithValue("listener", "admin"))
	})

	DescribeTable("certificate rejection log levels", func(cfg config.Config, failure string, level slog.Level) {
		DeferCleanup(log.SaveSettings(log.Log()))
		log.SetLevel(log.Log(), slog.LevelDebug)
		capture := installLogRecorder()
		DeferCleanup(capture.uninstall)
		server := newHTTPServer("admin", handler, &cfg)
		message := "http: TLS handshake error from 127.0.0.1:45122: " + failure
		server.inner.ErrorLog.Print(message)
		record := capture.hook.LastRecord()
		Expect(record.Level).Should(Equal(level))
		Expect(record.Message).Should(Equal(message))
		Expect(log.RecordFields(record)).Should(HaveKeyWithValue("listener", "admin"))
	},
		Entry("generated certificate rejected", config.Config{}, "remote error: tls: unknown certificate", slog.LevelDebug),
		Entry("generated certificate is untrusted", config.Config{}, "remote error: tls: bad certificate", slog.LevelDebug),
		Entry("generated certificate has an unknown CA", config.Config{}, "remote error: tls: unknown certificate authority", slog.LevelDebug),
		Entry("other handshake errors remain warnings", config.Config{}, "unexpected EOF", slog.LevelWarn),
		Entry("configured certificate rejected", config.Config{CertFile: "server.crt", KeyFile: "server.key"}, "remote error: tls: unknown certificate", slog.LevelWarn),
		Entry("configured certificate path only", config.Config{CertFile: "server.crt"}, "remote error: tls: unknown certificate", slog.LevelWarn),
		Entry("configured key path only", config.Config{KeyFile: "server.key"}, "remote error: tls: bad certificate", slog.LevelWarn),
		Entry("other certificate errors remain warnings", config.Config{}, "tls: failed to verify certificate: x509: certificate signed by unknown authority", slog.LevelWarn),
	)

	It("does not emit self-signed certificate rejection logs at info level", func() {
		DeferCleanup(log.SaveSettings(log.Log()))
		log.SetLevel(log.Log(), slog.LevelInfo)
		capture := installLogRecorder()
		DeferCleanup(capture.uninstall)
		server := newHTTPServer("admin", handler, &config.Config{})
		server.inner.ErrorLog.Print("http: TLS handshake error from 127.0.0.1:45122: remote error: tls: unknown certificate")
		Expect(capture.hook.Records()).Should(BeEmpty())
	})

	Describe("CORS", func() {
		preflight := func(target http.Handler, headers map[string]string) *http.Response {
			req := httptest.NewRequest(http.MethodOptions, "/dns-query", nil)
			req.Header.Set("Origin", "https://doh-client.example.com")
			req.Header.Set("Access-Control-Request-Method", http.MethodPost)

			for k, v := range headers {
				req.Header.Set(k, v)
			}

			rec := httptest.NewRecorder()
			target.ServeHTTP(rec, req)

			return rec.Result()
		}

		It("should not add CORS to common management middleware", func() {
			res := preflight(handler, nil)
			DeferCleanup(res.Body.Close)
			Expect(res.Header.Get("Access-Control-Allow-Origin")).Should(BeEmpty())
		})

		It("should answer a public DoH preflight without allowing credentials", func() {
			res := preflight(newCORSMiddleware()(handler), nil)
			DeferCleanup(res.Body.Close)

			Expect(res.Header.Get("Access-Control-Allow-Origin")).Should(Equal("*"))
			Expect(res.Header.Get("Access-Control-Allow-Methods")).Should(ContainSubstring(http.MethodPost))
			Expect(res.Header.Get("Access-Control-Allow-Credentials")).Should(BeEmpty())
		})

		It("should allow the DoH content type header", func() {
			res := preflight(newCORSMiddleware()(handler), map[string]string{
				"Access-Control-Request-Headers": "content-type",
			})
			DeferCleanup(res.Body.Close)

			Expect(res.Header.Get("Access-Control-Allow-Origin")).Should(Equal("*"))
			Expect(res.Header.Get("Access-Control-Allow-Headers")).Should(ContainSubstring("content-type"))
		})

		It("should not allow the Authorization header", func() {
			res := preflight(newCORSMiddleware()(handler), map[string]string{
				"Access-Control-Request-Headers": "authorization",
			})
			DeferCleanup(res.Body.Close)
			Expect(res.Header.Get("Access-Control-Allow-Origin")).Should(BeEmpty())
		})

		It("should allow Private Network Access preflights", func() {
			res := preflight(newCORSMiddleware()(handler), map[string]string{
				"Access-Control-Request-Private-Network": "true",
			})
			DeferCleanup(res.Body.Close)

			Expect(res.Header.Get("Access-Control-Allow-Private-Network")).Should(Equal("true"))
		})
	})

	It("should send security headers over HTTP as well as HTTPS", func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		Expect(rec.Header().Get("X-Frame-Options")).Should(Equal("DENY"))
		Expect(rec.Header().Get("X-Content-Type-Options")).Should(Equal("nosniff"))
		Expect(rec.Header().Get("Referrer-Policy")).Should(Equal("no-referrer"))
	})
})
