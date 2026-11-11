package server

import (
	"context"
	"net"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/creasty/defaults"
	"github.com/mia0x75/denyzer/config"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/require"
)

func TestBindRetryOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "address unavailable", err: syscall.EADDRNOTAVAIL},
		{name: "address in use", err: syscall.EADDRINUSE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var attempts int
				started := time.Now()
				err := retry.Do(func() error {
					attempts++

					return tc.err
				}, bindRetryOptions(t.Context(), networkTCP, "192.0.2.1:53")...)
				require.Equal(t, tc.err, err)
				require.Equal(t, 10, attempts)
				require.Equal(t, 4500*time.Millisecond, time.Since(started))
			})
		})
	}
}

func TestListenTCPRetryWaitsForAddressAndHonorsCancellation(t *testing.T) {
	blocker, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Close() })
	address := blocker.Addr().String()
	recorder := installLogRecorder()
	t.Cleanup(recorder.uninstall)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		listener, err := listenTCPRetry(ctx, address)
		if listener != nil {
			_ = listener.Close()
		}
		result <- err
	}()
	require.Eventually(t, func() bool {
		for _, message := range recorder.messages() {
			if strings.Contains(message, "could not bind tcp listener") {
				return true
			}
		}

		return false
	}, time.Second, 10*time.Millisecond, "bind retry did not occur")
	require.NoError(t, blocker.Close())
	select {
	case err := <-result:
		cancel()
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("bind retry did not succeed after address was released")
	}

	blockedAgain, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blockedAgain.Close()
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	started := time.Now()
	_, err = listenTCPRetry(ctx, blockedAgain.Addr().String())
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(started), time.Second)
}

func TestListenPacketRetryWaitsForAddress(t *testing.T) {
	blocker, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Close() })
	address := blocker.LocalAddr().String()
	recorder := installLogRecorder()
	t.Cleanup(recorder.uninstall)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		pc, err := listenPacketRetry(ctx, address)
		if pc != nil {
			_ = pc.Close()
		}
		result <- err
	}()
	require.Eventually(t, func() bool {
		for _, message := range recorder.messages() {
			if strings.Contains(message, "could not bind udp listener") {
				return true
			}
		}

		return false
	}, time.Second, 10*time.Millisecond, "bind retry did not occur")
	require.NoError(t, blocker.Close())
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("UDP bind retry did not succeed after address was released")
	}
}

func TestListenPacketRetryHonorsCancellation(t *testing.T) {
	blocker, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer blocker.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	started := time.Now()
	_, err = listenPacketRetry(ctx, blocker.LocalAddr().String())
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(started), time.Second)
}

func TestRetryServeClosesPreboundSocketsOnCancellation(t *testing.T) {
	for _, network := range []string{networkTCP, networkUDP} {
		t.Run(network, func(t *testing.T) {
			srv := &dns.Server{Net: network, Addr: "127.0.0.1:0", Handler: dns.NewServeMux()}
			require.NoError(t, bindDNSServer(t.Context(), srv))
			t.Cleanup(func() { closeServerSocket(srv) })
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			served := make(chan error, 1)
			go func() { served <- retryServe(ctx, srv) }()
			select {
			case err := <-served:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("cancelled DNS server did not stop")
			}
			if network == networkTCP {
				require.ErrorIs(t, srv.Listener.Close(), net.ErrClosed)
			} else {
				require.ErrorIs(t, srv.PacketConn.Close(), net.ErrClosed)
			}
		})
	}
}

func TestServerRetriesDNSBindUntilAddressIsAvailable(t *testing.T) {
	blocker, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Close() })
	address := blocker.Addr().String()
	cfg := &config.Config{}
	require.NoError(t, defaults.Set(cfg))
	cfg.Upstreams.Groups = map[string][]config.Upstream{
		"default": {{Net: config.NetProtocolTcpUdp, Host: "1.1.1.1", Port: 53}},
	}
	cfg.CustomDNS.Mapping = config.CustomDNSMapping{
		"custom.lan": {&dns.A{A: net.IPv4(192, 0, 2, 55)}},
	}
	cfg.Ports = config.Ports{DNS: config.ListenConfig{address}, DOHPath: "/dns-query"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv, err := NewServer(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	errCh := make(chan error, 4)
	recorder := installLogRecorder()
	t.Cleanup(recorder.uninstall)
	srv.Start(ctx, errCh)
	require.Eventually(t, func() bool {
		for _, message := range recorder.messages() {
			if strings.Contains(message, "could not bind tcp listener") {
				return true
			}
		}

		return false
	}, time.Second, 10*time.Millisecond, "bind retry did not occur")
	require.NoError(t, blocker.Close())

	var conn *dns.Conn
	require.Eventually(t, func() bool {
		var dialErr error
		conn, dialErr = dns.DialTimeout("tcp", address, 100*time.Millisecond)

		return dialErr == nil
	}, 6*time.Second, 50*time.Millisecond)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	query := new(dns.Msg)
	query.SetQuestion("custom.lan.", dns.TypeA)
	require.NoError(t, conn.WriteMsg(query))
	response, err := conn.ReadMsg()
	require.NoError(t, err)
	require.Equal(t, dns.RcodeSuccess, response.Rcode)
	require.Equal(t, query.Question, response.Question)
	require.Len(t, response.Answer, 1)
	require.IsType(t, &dns.A{}, response.Answer[0])
	answer := response.Answer[0].(*dns.A)
	require.Equal(t, "custom.lan.", answer.Hdr.Name)
	require.Equal(t, dns.TypeA, answer.Hdr.Rrtype)
	require.Equal(t, "192.0.2.55", answer.A.String())
	require.NoError(t, srv.Stop(context.Background()))
	require.Empty(t, errCh)
}
