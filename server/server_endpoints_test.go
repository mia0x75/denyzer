package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/go-chi/chi/v5"
	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/resolver"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestDoHRejectsNonQueries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(*dns.Msg)
	}{
		{"update", func(m *dns.Msg) {
			m.SetUpdate("example.")
			m.Insert([]dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "host.example.", Rrtype: dns.TypeA, Class: dns.ClassINET}, A: net.IPv4(192, 0, 2, 1)}})
		}},
		{"response", func(m *dns.Msg) { m.Response = true }},
		{"notify", func(m *dns.Msg) { m.Opcode = dns.OpcodeNotify }},
		{"no questions", func(m *dns.Msg) { m.Question = nil }},
		{"multiple questions", func(m *dns.Msg) { m.Question = append(m.Question, m.Question[0]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := new(dns.Msg).SetQuestion("host.example.", dns.TypeA)
			tc.edit(query)
			raw, err := query.Pack()
			require.NoError(t, err)
			upstream := resolver.NewMockChainedResolver(t)
			srv := &Server{queryResolver: upstream}
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				t.Run(method, func(t *testing.T) {
					request := httptest.NewRequestWithContext(t.Context(), method,
						"/dns-query?dns="+base64.RawURLEncoding.EncodeToString(raw), bytes.NewReader(raw))
					request.Header.Set(contentTypeHeader, dnsContentType)
					recorder := httptest.NewRecorder()
					if method == http.MethodGet {
						srv.dohGetRequestHandler(recorder, request)
					} else {
						srv.dohPostRequestHandler(recorder, request)
					}
					require.Equal(t, http.StatusBadRequest, recorder.Code)
				})
			}
			upstream.AssertNotCalled(t, "Resolve", mock.Anything, mock.Anything)
		})
	}
}

func TestDoHClientIDLimit(t *testing.T) {
	t.Parallel()
	query, err := new(dns.Msg).SetQuestion("host.example.", dns.TypeA).Pack()
	require.NoError(t, err)
	for _, tc := range []struct {
		name, path, host string
	}{
		{"oversized path", "/dns-query/" + strings.Repeat("x", 900*1024), "example.test"},
		{"oversized host ID", "/dns-query", "id-" + strings.Repeat("x", maxClientIDLength+1) + ".example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := resolver.NewMockChainedResolver(t)
			srv := &Server{queryResolver: upstream}
			router := chi.NewRouter()
			srv.registerDoHEndpoints(router, &config.Config{Ports: config.Ports{DOHPath: "/dns-query"}})
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, tc.path, bytes.NewReader(query))
			req.Host = tc.host
			req.Header.Set(contentTypeHeader, dnsContentType)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			upstream.AssertNotCalled(t, "Resolve", mock.Anything, mock.Anything)
		})
	}
}

func TestHTTPClientIDOwnsBoundedStorage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path, host, want string
	}{
		{"maximum path", "/dns-query/" + strings.Repeat("x", maxClientIDLength), "example.test", strings.Repeat("x", maxClientIDLength)},
		{"short host prefix", "/dns-query", "id-client." + strings.Repeat("x", 900*1024), "client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := chi.NewRouter()
			var request *model.Request
			var parseErr error
			var source string
			handler := func(w http.ResponseWriter, r *http.Request) {
				source = chi.URLParam(r, "clientID")
				if source == "" {
					source = extractClientIDFromHost(r.Host)
				}
				_, request, parseErr = newRequestFromHTTP(r.Context(), r, new(dns.Msg))
				w.WriteHeader(http.StatusOK)
			}
			router.Get("/dns-query", handler)
			router.Get("/dns-query/{clientID}", handler)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil)
			req.Host = tc.host
			router.ServeHTTP(httptest.NewRecorder(), req)
			require.NoError(t, parseErr)
			require.Equal(t, tc.want, request.RequestClientID)
			require.NotSame(t, unsafe.StringData(source), unsafe.StringData(request.RequestClientID))
		})
	}
}

func TestDoHResponsesKeepClientPoliciesPrivate(t *testing.T) {
	t.Parallel()
	upstream := resolver.NewMockChainedResolver(t)
	upstream.EXPECT().Resolve(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, request *model.Request) (*model.Response, error) {
			answer := new(dns.Msg).SetReply(request.Req)
			ip := net.IPv4(192, 0, 2, 10)
			if request.ClientIP.Equal(net.ParseIP("192.0.2.2")) {
				ip = net.IPv4zero
			}
			answer.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: "host.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: ip}}

			return &model.Response{Res: answer}, nil
		}).Twice()
	srv := &Server{cfg: &config.Config{Upstreams: config.Upstreams{Timeout: config.Duration(time.Second)}}, queryResolver: upstream}
	query, err := new(dns.Msg).SetQuestion("host.example.", dns.TypeA).Pack()
	require.NoError(t, err)
	url := "/dns-query?dns=" + base64.RawURLEncoding.EncodeToString(query)
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		req.RemoteAddr = net.JoinHostPort(ip, "1234")
		rec := httptest.NewRecorder()
		srv.dohGetRequestHandler(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "private, max-age=60", rec.Header().Get(cacheControlHeader))
		answer := new(dns.Msg)
		require.NoError(t, answer.Unpack(rec.Body.Bytes()))
		require.Len(t, answer.Answer, 1)
		require.Equal(t, ip == "192.0.2.2", answer.Answer[0].(*dns.A).A.Equal(net.IPv4zero))
	}
}

func TestGetSmallestTTLFromAnswer(t *testing.T) {
	tests := []struct {
		name string
		ttls []uint32
		want uint32
	}{
		{"empty answer", nil, 0},
		{"single zero", []uint32{0}, 0},
		{"zero first", []uint32{0, 60}, 0},
		{"zero last", []uint32{60, 0}, 0},
		{"zero in middle", []uint32{60, 0, 30}, 0},
		{"positive", []uint32{60, 30, 120}, 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := new(dns.Msg)
			for i, ttl := range tt.ttls {
				msg.Answer = append(msg.Answer, &dns.A{
					Hdr: dns.RR_Header{
						Name:   fmt.Sprintf("r%d.example.", i),
						Rrtype: dns.TypeA,
						Class:  dns.ClassINET,
						Ttl:    ttl,
					},
					A: net.ParseIP("192.0.2.1"),
				})
			}

			if got := getSmallestTTLFromAnswer(msg); got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}
