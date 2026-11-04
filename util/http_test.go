package util

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("HTTP Util", func() {
	Describe("DefaultHTTPTransport", func() {
		It("returns a new transport", func() {
			a := DefaultHTTPTransport()
			Expect(a).Should(BeIdenticalTo(a))

			b := DefaultHTTPTransport()
			Expect(a).ShouldNot(BeIdenticalTo(b))
		})

		It("returns a copy of http.DefaultTransport", func() {
			Expect(cmp.Diff(
				DefaultHTTPTransport(), http.DefaultTransport,
				cmpopts.IgnoreUnexported(http.Transport{}),
				// Non nil func field comparers
				cmp.Comparer(cmpAsPtrs[func(context.Context, string, string) (net.Conn, error)]),
				cmp.Comparer(cmpAsPtrs[func(*http.Request) (*url.URL, error)]),
			)).Should(BeEmpty())
		})

		It("copies every field from its namesake", func() {
			// http.DefaultTransport leaves most fields at their zero value, so a
			// field copied from the wrong source is invisible when comparing
			// against it: 0 == 0. Compare against a base whose fields all hold
			// distinct values instead, which pins each copy to its own namesake.
			DeferCleanup(func(orig *http.Transport) { baseTransport = orig }, baseTransport)

			baseTransport = &http.Transport{
				DisableCompression:     true,
				DisableKeepAlives:      false,
				ForceAttemptHTTP2:      true,
				ExpectContinueTimeout:  1 * time.Second,
				IdleConnTimeout:        2 * time.Second,
				ResponseHeaderTimeout:  3 * time.Second,
				TLSHandshakeTimeout:    4 * time.Second,
				MaxConnsPerHost:        11,
				MaxIdleConns:           22,
				MaxIdleConnsPerHost:    33,
				MaxResponseHeaderBytes: 44,
				ReadBufferSize:         55,
				WriteBufferSize:        66,
				ProxyConnectHeader:     http.Header{"X-Test": []string{"1"}},
			}

			Expect(cmp.Diff(
				DefaultHTTPTransport(), baseTransport,
				cmpopts.IgnoreUnexported(http.Transport{}),
			)).Should(BeEmpty())
		})
	})

	DescribeTable("HTTPClientIP ignores spoofed forwarding headers",
		func(remoteAddr, expected string) {
			r, err := http.NewRequest(http.MethodGet, "http://example.com", nil)
			Expect(err).Should(Succeed())
			r.RemoteAddr = remoteAddr
			r.Header.Set("Forwarded", `for="[2001:db8::99]:8080", for=203.0.113.99`)
			r.Header.Set("X-Forwarded-For", "203.0.113.99, 192.0.2.99")

			Expect(HTTPClientIP(r)).Should(Equal(net.ParseIP(expected)))
		},
		Entry("IPv4 with port", "192.0.2.1:12345", "192.0.2.1"),
		Entry("IPv4 without port", "192.0.2.1", "192.0.2.1"),
		Entry("IPv6 with port", "[2001:db8::1]:12345", "2001:db8::1"),
		Entry("IPv6 without port", "2001:db8::1", "2001:db8::1"),
		Entry("invalid peer", "not-an-ip:12345", ""),
		Entry("missing peer", "", ""),
	)
})

// Go and cmp don't define func comparisons, besides with nil.
// In practice we can just compare them as pointers.
func cmpAsPtrs[T any](x, y T) bool {
	return reflect.ValueOf(x).Pointer() == reflect.ValueOf(y).Pointer()
}
