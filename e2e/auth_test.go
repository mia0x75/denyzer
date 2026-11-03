package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/mia0x75/denyzer/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Management client", func() {
	It("logs in over TLS and uses Bearer authorization without following redirects", func(ctx context.Context) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			Expect(r.TLS).ShouldNot(BeNil())
			if r.URL.Path == "/api/login" {
				Expect(r.Method).Should(Equal(http.MethodPost))
				var credentials map[string]string
				Expect(json.NewDecoder(r.Body).Decode(&credentials)).Should(Succeed())
				Expect(credentials).Should(Equal(map[string]string{"username": "admin", "password": e2ePassword}))
				_, err := w.Write([]byte(`{"token":"test-only-token"}`))
				Expect(err).Should(Succeed())

				return
			}
			Expect(r.Header.Get("Authorization")).Should(Equal("Bearer test-only-token"))
			w.Header().Set("Location", "https://other-origin.invalid/")
			w.WriteHeader(http.StatusSeeOther)
		}))
		DeferCleanup(server.Close)
		client, err := newManagementClient(ctx, server.URL)
		Expect(err).Should(Succeed())
		response, err := client.Get(server.URL + "/api/status")
		Expect(err).Should(Succeed())
		DeferCleanup(response.Body.Close)
		Expect(response.StatusCode).Should(Equal(http.StatusSeeOther))
	})

	DescribeTable("rejects unusable login responses", func(ctx context.Context, status int, body string) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			defer GinkgoRecover()
			w.WriteHeader(status)
			_, err := w.Write([]byte(body))
			Expect(err).Should(Succeed())
		}))
		DeferCleanup(server.Close)
		_, err := newManagementClient(ctx, server.URL)
		Expect(err).Should(HaveOccurred())
	}, Entry("invalid credentials", http.StatusUnauthorized, "invalid credentials"),
		Entry("invalid JSON", http.StatusOK, "{"), Entry("empty token", http.StatusOK, `{}`))

	It("writes test credentials without losing the fixture configuration", func() {
		path := createTempFile("ports:", "  admin: 4000", "customDNS:", "  mapping:", "    fixture.test: 192.0.2.1")
		Expect(setManagementAuth(path)).Should(Succeed())
		cfg, err := config.LoadConfig(path, true)
		Expect(err).Should(Succeed())
		Expect(cfg.Auth.IsEnabled()).Should(BeTrue())
		Expect(cfg.Auth.Username).Should(Equal("admin"))
		Expect(cfg.Auth.PasswordHash).Should(HavePrefix("$argon2id$"))
		Expect(cfg.Ports.Admin).Should(Equal(config.ListenConfig{":4000"}))
		Expect(cfg.CustomDNS.Mapping).Should(HaveKey("fixture.test"))
	})

	It("probes the actual HTTPS endpoint rather than the removed HTTP root", func(ctx context.Context) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, "/api/login") {
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		DeferCleanup(server.Close)
		Expect(doHTTPRequest(ctx, server.URL+"/api/login")).Should(Succeed())
		Expect(doHTTPRequest(ctx, server.URL+"/")).Should(HaveOccurred())
	})
})
