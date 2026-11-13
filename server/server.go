package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/avast/retry-go/v4"
	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/resolver"
	"github.com/mia0x75/denyzer/server/freebind"

	"github.com/hashicorp/go-multierror"
	"github.com/mia0x75/denyzer/util"

	"github.com/go-chi/chi/v5"
	"github.com/miekg/dns"
	"github.com/pires/go-proxyproto"
	"github.com/quic-go/quic-go"
)

const (
	maxUDPBufferSize = 65535
	caExpiryYears    = 10
	certExpiryYears  = 5

	networkUDP    = "udp"
	networkTCP    = "tcp"
	networkTCPTLS = "tcp-tls"
	httpsName     = "https"
)

// Server controls the endpoints for DNS and HTTP
type Server struct {
	dnsServers    []*dns.Server
	queryResolver resolver.ChainedResolver
	cfg           *config.Config

	servers          map[net.Listener]*httpServer
	serverConfigs    map[string]*httpServer
	serverTLSConfig  *tls.Config
	http3Server      *http3Server     // nil when disabled
	http3PacketConns []net.PacketConn // one per address in ports.https
	httpServeWG      sync.WaitGroup
	httpMu           sync.Mutex

	// dnsStartCancels holds the cancel funcs of every DNS startup context
	// created by Start; Stop cancels all of them to abort bind retries and
	// stop the serve loops. Keep the upstream slice representation; repeated
	// Start calls are rejected before another context or worker is created.
	dnsStartCancels []context.CancelFunc
	dnsCancelMu     sync.Mutex     // guards startup state and worker registration
	dnsServeWG      sync.WaitGroup // tracks the DNS serve goroutines started in Start
	dnsStarted      bool
	// The DNS startup state above is populated by Start and read by Stop.
	// dnsCancelMu serializes worker registration with shutdown so Start and
	// Stop may run concurrently without racing WaitGroup registration.
	stopped atomic.Bool // guards against a double Stop
	bindWG  sync.WaitGroup
}

func logger() *slog.Logger {
	return log.PrefixedLog("server")
}

func tlsCipherSuites() []uint16 {
	tlsCipherSuites := []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
	}

	return tlsCipherSuites
}

type NewServerFunc func(address string) (*dns.Server, error)

func retrieveCertificate(cfg *config.Config) (cert tls.Certificate, err error) {
	if cfg.CertFile == "" && cfg.KeyFile == "" {
		cert, err = util.TLSGenerateSelfSignedCert([]string{"denyzer.invalid", "*"})
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("unable to generate self-signed certificate: %w", err)
		}

		log.Log().Warn("using self-signed certificate; configure certFile/keyFile with a trusted certificate " +
			"matching the access hostname or IP")
	} else {
		cert, err = tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("can't load certificate files: %w", err)
		}
	}

	return cert, nil
}

func newTLSConfig(cfg *config.Config) (*tls.Config, error) {
	var cert tls.Certificate

	cert, err := retrieveCertificate(cfg)
	if err != nil {
		return nil, fmt.Errorf("can't retrieve cert: %w", err)
	}

	// #nosec G402 // See TLSVersion.validate
	res := &tls.Config{
		MinVersion:   uint16(cfg.MinTLSServeVer), //nolint:gosec // TLS version constants fit safely in uint16
		CipherSuites: tlsCipherSuites(),
		Certificates: []tls.Certificate{cert},
	}

	return res, nil
}

// NewServer creates new server instance with passed config
//
//nolint:funlen // Keep listener construction and rollback together.
func NewServer(ctx context.Context, cfg *config.Config) (server *Server, err error) {
	if err := cfg.ValidateAdminListeners(logger()); err != nil {
		return nil, err
	}
	var tlsCfg *tls.Config

	if len(cfg.Ports.Admin) > 0 || len(cfg.Ports.HTTPS) > 0 || len(cfg.Ports.TLS) > 0 {
		tlsCfg, err = newTLSConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create TLS configuration: %w", err)
		}
	}

	if cfg.Ports.FreeBind && !freebind.Supported {
		logger().WarnContext(ctx, "ports.freeBind: true is only supported on Linux; "+
			"ignoring on this platform (binding normally)")
	}

	dnsServers, err := createServers(ctx, cfg, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("server creation failed: %w", err)
	}

	var http3PacketConns []net.PacketConn
	defer func() {
		if err != nil {
			closeDNSServers(dnsServers)
			closeAll(http3PacketConns)
		}
	}()

	http3PacketConns, err = createHTTP3PacketConns(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP/3 UDP listeners: %w", err)
	}

	bootstrap, err := resolver.NewBootstrap(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create bootstrap resolver: %w", err)
	}

	queryResolver, queryError := createQueryResolver(ctx, cfg, bootstrap)
	if queryError != nil {
		return nil, queryError
	}

	server = &Server{
		dnsServers:       dnsServers,
		queryResolver:    queryResolver,
		cfg:              cfg,
		servers:          make(map[net.Listener]*httpServer),
		serverConfigs:    make(map[string]*httpServer),
		serverTLSConfig:  tlsCfg,
		http3PacketConns: http3PacketConns,
	}
	server.printConfiguration()

	server.registerDNSHandlers(ctx)

	// Public DNS listeners never register management endpoints or authentication middleware.
	httpRouter := chi.NewRouter()
	httpRouter.Use(newCORSMiddleware())
	server.registerDoHEndpoints(httpRouter, cfg)
	if len(cfg.Ports.Admin) > 0 {
		adminHandler, err := server.createAdminHandler()
		if err != nil {
			return nil, err
		}
		server.serverConfigs["admin"] = newHTTPServer("admin", adminHandler, cfg)
	}

	if len(http3PacketConns) > 0 {
		server.http3Server = newHTTP3Server(httpRouter, newH3TLSConfig(tlsCfg))
	}

	if len(cfg.Ports.HTTP) > 0 {
		server.serverConfigs["http"] = newHTTPServer("http", httpRouter, cfg)
	}

	if len(cfg.Ports.HTTPS) != 0 {
		var httpsHandler http.Handler = httpRouter
		if server.http3Server != nil {
			httpsHandler = newAltSvcMiddleware(server.http3Server)(httpRouter)
		}

		server.serverConfigs["https"] = newHTTPServer("https", httpsHandler, cfg)
	}

	return server, err
}

func createServers(ctx context.Context, cfg *config.Config, tlsCfg *tls.Config) ([]*dns.Server, error) {
	var dnsServers []*dns.Server

	var err *multierror.Error

	freeBind := cfg.Ports.FreeBind

	addServers := func(newServer NewServerFunc, addresses config.ListenConfig) error {
		for _, address := range addresses {
			server, err := newServer(address)
			if err != nil {
				return err
			}

			dnsServers = append(dnsServers, server)
		}

		return nil
	}

	err = multierror.Append(err,
		addServers(func(address string) (*dns.Server, error) {
			return createUDPServer(ctx, address, listenerOptions{freeBind: freeBind})
		}, cfg.Ports.DNS),
		addServers(func(address string) (*dns.Server, error) {
			return createTCPServer(ctx, address, listenerOptions{
				freeBind:      freeBind,
				proxyProtocol: cfg.Ports.ProxyProtocol.Has(config.ProxyProtocolTypeDns),
			})
		}, cfg.Ports.DNS),
		addServers(func(address string) (*dns.Server, error) {
			return createTLSServer(ctx, address, tlsCfg, listenerOptions{
				freeBind:      freeBind,
				proxyProtocol: cfg.Ports.ProxyProtocol.Has(config.ProxyProtocolTypeTls),
			})
		}, cfg.Ports.TLS))

	if multiErr := err.ErrorOrNil(); multiErr != nil {
		closeDNSServers(dnsServers)

		return nil, fmt.Errorf("failed to create DNS servers: %w", multiErr)
	}

	return dnsServers, nil
}

func createHTTP3PacketConns(ctx context.Context, cfg *config.Config) ([]net.PacketConn, error) {
	if cfg.HTTP3.IsEnabled() {
		switch {
		case len(cfg.Ports.HTTPS) == 0:
			logger().WarnContext(ctx, "http3.enable is true but ports.https is empty; HTTP/3 disabled")
		case cfg.Ports.ProxyProtocol.Has(config.ProxyProtocolTypeHttps):
			logger().WarnContext(ctx, "http3.enable is true but ports.proxyProtocol includes 'https'; "+
				"HTTP/3 cannot carry PROXY protocol headers and is disabled to keep the client IP consistent")
		default:
			http3PacketConns, err := newUDPPacketConns(ctx, cfg.Ports.HTTPS)
			if err != nil {
				return nil, fmt.Errorf("failed to create HTTP/3 UDP listeners: %w", err)
			}

			return http3PacketConns, nil
		}
	}

	return nil, nil
}

func closeAll[T io.Closer](closers []T) {
	for _, c := range closers {
		_ = c.Close()
	}
}

func closeDNSServers(servers []*dns.Server) {
	for _, server := range servers {
		closeServerSocket(server)
	}
}

func newTCPListeners(
	ctx context.Context, proto string, addresses config.ListenConfig, proxyProtocol bool,
) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(addresses))

	for _, address := range addresses {
		listener, err := listenTCPRetry(ctx, address)
		if err != nil {
			closeAll(listeners)

			return nil, fmt.Errorf("start %s listener on %s failed: %w", proto, address, err)
		}

		listener = newProxyProtocolListener(listener, proxyProtocol)

		listeners = append(listeners, listener)
	}

	return listeners, nil
}

func newTLSListeners(
	ctx context.Context, proto string, addresses config.ListenConfig, tlsCfg *tls.Config, proxyProtocol bool,
) ([]net.Listener, error) {
	listeners, err := newTCPListeners(ctx, proto, addresses, proxyProtocol)
	if err != nil {
		return nil, fmt.Errorf("failed to create TCP listeners for TLS: %w", err)
	}

	for i, inner := range listeners {
		listeners[i] = tls.NewListener(inner, tlsCfg)
	}

	return listeners, nil
}

func newProxyProtocolListener(listener net.Listener, enabled bool) net.Listener {
	if !enabled {
		return listener
	}

	return &proxyproto.Listener{
		Listener: listener,
		ConnPolicy: func(proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			return proxyproto.REQUIRE, nil
		},
	}
}

// listenerOptions bundles the socket-level options applied when a DNS listener is pre-created
// before miekg/dns starts serving (freebind socket option, PROXY protocol wrapping).
type listenerOptions struct {
	freeBind      bool
	proxyProtocol bool
}

func createDNSServer(ctx context.Context, network, address string, tlsCfg *tls.Config, opts listenerOptions,
) (*dns.Server, error) {
	srv := &dns.Server{
		Addr:    address,
		Net:     network,
		Handler: dns.NewServeMux(),
	}
	srv.NotifyStartedFunc = func() {
		var boundAddress net.Addr
		if network == networkUDP {
			boundAddress = srv.PacketConn.LocalAddr()
		} else {
			boundAddress = srv.Listener.Addr()
		}
		logger().InfoContext(ctx, fmt.Sprintf("%s server is up and running on address %s",
			strings.ToUpper(network), boundAddress))
	}

	if network == networkUDP {
		srv.UDPSize = maxUDPBufferSize
	}

	if tlsCfg != nil {
		srv.TLSConfig = tlsCfg
	}

	// When freeBind is enabled (and supported), pre-create the listener with the IP_FREEBIND socket
	// option and hand it to the server, which is then started via ActivateAndServe (see retryServe).
	// PROXY protocol listeners also need pre-created sockets so they can be wrapped before serving.
	if (opts.freeBind && freebind.Supported) || (opts.proxyProtocol && network != networkUDP) {
		if err := attachListener(ctx, srv, network, address, tlsCfg, listenerOptions{
			freeBind:      opts.freeBind && freebind.Supported,
			proxyProtocol: opts.proxyProtocol,
		}); err != nil {
			return nil, err
		}
	}

	return srv, nil
}

// attachListener creates a listener/packet connection for DNS servers that need custom socket handling
// before miekg/dns starts serving, such as freebind or PROXY protocol wrapping.
func attachListener(ctx context.Context, srv *dns.Server, network, address string,
	tlsCfg *tls.Config, opts listenerOptions,
) error {
	lc := net.ListenConfig{}
	if opts.freeBind {
		lc.Control = freebind.Control
	}

	switch network {
	case networkUDP:
		var pc net.PacketConn
		err := retry.Do(func() error {
			var err error
			pc, err = lc.ListenPacket(ctx, networkUDP, address)

			return err
		}, bindRetryOptions(ctx, networkUDP, address)...)
		if err != nil {
			return fmt.Errorf("freebind udp listener on %s failed: %w", address, err)
		}

		srv.PacketConn = pc
	case networkTCP:
		var l net.Listener
		err := retry.Do(func() error {
			var err error
			l, err = lc.Listen(ctx, networkTCP, address)

			return err
		}, bindRetryOptions(ctx, networkTCP, address)...)
		if err != nil {
			return fmt.Errorf("tcp listener on %s failed: %w", address, err)
		}

		l = newProxyProtocolListener(l, opts.proxyProtocol)
		srv.Listener = l
	case networkTCPTLS:
		var l net.Listener
		err := retry.Do(func() error {
			var err error
			l, err = lc.Listen(ctx, networkTCP, address)

			return err
		}, bindRetryOptions(ctx, networkTCPTLS, address)...)
		if err != nil {
			return fmt.Errorf("tcp-tls listener on %s failed: %w", address, err)
		}

		l = newProxyProtocolListener(l, opts.proxyProtocol)
		srv.Listener = tls.NewListener(l, tlsCfg)
	default:
		return fmt.Errorf("unsupported DNS listener network %q", network)
	}

	return nil
}

func createTLSServer(ctx context.Context, address string, tlsCfg *tls.Config, opts listenerOptions,
) (*dns.Server, error) {
	return createDNSServer(ctx, networkTCPTLS, address, tlsCfg, opts)
}

func createTCPServer(ctx context.Context, address string, opts listenerOptions) (*dns.Server, error) {
	return createDNSServer(ctx, networkTCP, address, nil, opts)
}

func createUDPServer(ctx context.Context, address string, opts listenerOptions) (*dns.Server, error) {
	return createDNSServer(ctx, networkUDP, address, nil, opts)
}

// queryResolverParts holds the resolvers that make up the query chain, together
// with their construction errors. Its fields are filled concurrently, so it is
// only safe to read them once newQueryResolverParts has returned.
type queryResolverParts struct {
	upstreamTree    resolver.Resolver
	blocking        *resolver.BlockingResolver
	queryLogging    *resolver.QueryLoggingResolver
	condUpstream    *resolver.ConditionalUpstreamResolver
	customDNS       *resolver.CustomDNSResolver
	hostsFile       *resolver.HostsFileResolver
	clientNames     *resolver.ClientNamesResolver
	cachingResolver *resolver.CachingResolver
	dnssecResolver  resolver.ChainedResolver

	utErr error
	blErr error
	qlErr error
	cuErr error
	hfErr error
	cnErr error
	crErr error
	dsErr error
}

// newQueryResolverParts constructs the resolvers of the query chain.
//
// The resolvers that only depend on their own configuration are built
// concurrently: several of them do real work (list downloads, upstream
// verification, opening the query log), and running them one after the other
// makes startup the sum of their initialisation times. Client-name resolution
// and DNSSEC validation depend on those results, so they form a second concurrent
// stage.
func newQueryResolverParts(
	ctx context.Context,
	cfg *config.Config,
	bootstrap *resolver.Bootstrap,
) *queryResolverParts {
	parts := &queryResolverParts{
		customDNS: resolver.NewCustomDNSResolver(cfg.CustomDNS),
	}

	runConcurrently([]func(){
		func() {
			parts.upstreamTree, parts.utErr = resolver.NewUpstreamTreeResolver(ctx, cfg.Upstreams, bootstrap)
		},
		func() {
			parts.blocking, parts.blErr = resolver.NewBlockingResolver(ctx, cfg.Blocking, bootstrap)
		},
		func() {
			parts.queryLogging, parts.qlErr = resolver.NewQueryLoggingResolver(ctx, cfg.QueryLog)
		},
		func() {
			parts.condUpstream, parts.cuErr = resolver.NewConditionalUpstreamResolver(
				ctx, cfg.Conditional, cfg.Upstreams, bootstrap)
		},
		func() {
			parts.hostsFile, parts.hfErr = resolver.NewHostsFileResolver(ctx, cfg.HostsFile, bootstrap)
		},
		func() {
			parts.cachingResolver, parts.crErr = resolver.NewCachingResolver(ctx, cfg.Caching, cfg.DNSSEC)
		},
	})

	// Second stage: these consume the resolvers built above.
	runConcurrently([]func(){
		func() {
			// client name resolution consults local reverse sources (custom DNS, hosts file)
			// before the rDNS upstream
			parts.clientNames, parts.cnErr = resolver.NewClientNamesResolver(
				ctx, cfg.ClientLookup, cfg.Upstreams, bootstrap, parts.customDNS, parts.hostsFile)
		},
		func() {
			// UpstreamTree is passed to the DNSSEC resolver so it can query for DNSKEY/DS records
			parts.dnssecResolver, parts.dsErr = resolver.NewDNSSECResolver(ctx, cfg.DNSSEC, parts.upstreamTree)
		},
	})

	return parts
}

// runConcurrently runs every job in its own goroutine and returns once all of
// them have finished.
func runConcurrently(jobs []func()) {
	var wg sync.WaitGroup

	wg.Add(len(jobs))

	for _, job := range jobs {
		go func() {
			defer wg.Done()

			job()
		}()
	}

	wg.Wait()
}

func createQueryResolver(
	ctx context.Context,
	cfg *config.Config,
	bootstrap *resolver.Bootstrap,
) (resolver.ChainedResolver, error) {
	parts := newQueryResolverParts(ctx, cfg, bootstrap)

	multiErr := multierror.Append(
		multierror.Prefix(parts.utErr, "upstream tree resolver: "),
		multierror.Prefix(parts.blErr, "blocking resolver: "),
		multierror.Prefix(parts.qlErr, "query logging resolver: "),
		multierror.Prefix(parts.cnErr, "client names resolver: "),
		multierror.Prefix(parts.cuErr, "conditional upstream resolver: "),
		multierror.Prefix(parts.hfErr, "hosts file resolver: "),
		multierror.Prefix(parts.crErr, "caching resolver: "),
		multierror.Prefix(parts.dsErr, "dnssec resolver: "),
	).ErrorOrNil()
	if multiErr != nil {
		return nil, fmt.Errorf("failed to create query resolver components: %w", multiErr)
	}

	r := resolver.Chain(
		resolver.NewStatsResolver(ctx, cfg.Statistics),
		// stays above the ECS and client-name lookups: its bucket key must remain the
		// connection's source IP. Keyed on the ECS address instead (ecs.useAsClient), the
		// key would be attacker-controlled, letting a client both evade its own bucket and
		// fill the bounded bucket store, which drops queries of every new client once full.
		// Dropped queries therefore carry no client name and are attributed to the client
		// IP in the statistics.
		resolver.NewRateLimitingResolver(ctx, cfg.RateLimit),
		// adopts the ECS subnet as the internal client IP (ecs.useAsClient) before the
		// client-name lookup, blocking and the cache consume the client identity, so the
		// ECS client is used for those features and is preserved across cache hits
		resolver.NewECSClientResolver(cfg.ECS),
		// above filtering and fqdnOnly, which answer a query on their own: a lookup below
		// them would leave request.ClientNames empty for every query they short-circuit,
		// and the statistics would attribute those to the raw client IP while the same
		// client's other queries are attributed to its name
		parts.clientNames,
		parts.queryLogging,
		resolver.NewFilteringResolver(cfg.Filtering),
		resolver.NewFQDNOnlyResolver(cfg.FQDNOnly),
		resolver.NewEDEResolver(cfg.EDE),
		parts.customDNS,
		parts.hostsFile,
		// above blocking and the cache: it inspects only RESOLVED/CACHED answers
		// (conditional/custom DNS/hosts file/SUDN/blocked answers are recognized
		// by response type and pass through; the cache stores only
		// upstream-derived answers, so CACHED implies upstream origin), cached
		// answers — are re-inspected on every
		// hit, and blocking's internal FQDN client-identifier lookups enter the
		// chain below it
		resolver.NewRebindingProtectionResolver(cfg.RebindingProtection),
		parts.blocking,
		parts.dnssecResolver, // DNSSEC validation BEFORE caching - validates all responses before they are cached
		parts.cachingResolver,
		resolver.NewDNS64Resolver(cfg.DNS64), // DNS64 synthesis AFTER caching
		resolver.NewECSResolver(cfg.ECS),
		parts.condUpstream,
		resolver.NewSpecialUseDomainNamesResolver(cfg.SUDN),
		parts.upstreamTree,
	)

	return r, nil
}

func (s *Server) registerDNSHandlers(ctx context.Context) {
	for _, server := range s.dnsServers {
		//nolint:forcetypeassert // handler is always *dns.ServeMux; set during server construction
		handler := server.Handler.(*dns.ServeMux)
		handler.HandleFunc(".", func(w dns.ResponseWriter, m *dns.Msg) {
			s.OnRequest(ctx, w, m)
		})
		handler.HandleFunc("healthcheck.denyzer", func(w dns.ResponseWriter, m *dns.Msg) {
			s.OnHealthCheck(ctx, w, m)
		})
	}
}

func (s *Server) printConfiguration() {
	logger().Info("current configuration:")

	resolver.ForEach(s.queryResolver, func(res resolver.Resolver) {
		resolver.LogResolverConfig(res, logger())
	})

	logger().Info("listeners:")
	log.WithIndent(logger(), "  ", s.cfg.Ports.LogConfig)

	if len(s.http3PacketConns) > 0 {
		logger().Info("HTTP/3:")
		log.WithIndent(logger(), "  ", s.cfg.HTTP3.LogConfig)
	}

	logger().Info("runtime information:")

	// force garbage collector
	runtime.GC()
	debug.FreeOSMemory()

	logger().Info(fmt.Sprintf("  numCPU =       %d", runtime.NumCPU()))
	logger().Info(fmt.Sprintf("  numGoroutine = %d", runtime.NumGoroutine()))

	// gather memory stats
	var m runtime.MemStats

	runtime.ReadMemStats(&m)

	logger().Info("  memory:")
	logger().Info(fmt.Sprintf("    heap =     %10v MB", toMB(m.HeapAlloc)))
	logger().Info(fmt.Sprintf("    sys =      %10v MB", toMB(m.Sys)))
	logger().Info(fmt.Sprintf("    numGC =    %10v", m.NumGC))
}

func toMB(b uint64) uint64 {
	const bytesInKB = 1024

	return b / bytesInKB / bytesInKB
}

// Start starts the server
//
//nolint:funlen,gocognit,gocyclo // Preserve the upstream startup layout to ease future merges.
func (s *Server) Start(ctx context.Context, errCh chan<- error) {
	logger().InfoContext(ctx, "Starting server")
	s.dnsCancelMu.Lock()
	if s.stopped.Load() {
		s.dnsCancelMu.Unlock()
		select {
		case errCh <- errors.New("server already stopped"):
		case <-ctx.Done():
		}

		return
	}
	if s.dnsStarted {
		s.dnsCancelMu.Unlock()
		select {
		case errCh <- errors.New("server already started"):
		case <-ctx.Done():
		}

		return
	}
	s.dnsStarted = true
	// dnsCtx is cancelled by Stop to abort in-progress bind retries and stop
	// the DNS serve loops (see retryServe). It also cancels deferred HTTP binds.
	dnsCtx, cancel := context.WithCancel(ctx)
	s.dnsStartCancels = append(s.dnsStartCancels, cancel)
	s.bindWG.Add(len(s.dnsServers))

	for _, srv := range s.dnsServers {
		ready := sync.OnceFunc(s.bindWG.Done)
		s.dnsServeWG.Go(func() {
			defer ready()
			notify := srv.NotifyStartedFunc
			srv.NotifyStartedFunc = func() {
				if notify != nil {
					notify()
				}
				ready()
			}
			if err := retryServe(dnsCtx, srv); err != nil && dnsCtx.Err() == nil {
				// The error send is cancellation-aware: if the channel has no
				// reader (or is full) while Stop cancels dnsCtx, we must not
				// block, otherwise the serve goroutine never finishes and
				// dnsServeWG.Wait() in Stop hangs past its deadline.
				select {
				case errCh <- fmt.Errorf("start %s listener failed: %w", srv.Net, err):
				case <-dnsCtx.Done():
				}
			}
		})
	}

	for name, srv := range s.serverConfigs {
		addresses := s.cfg.Ports.HTTP
		switch name {
		case httpsName:
			addresses = s.cfg.Ports.HTTPS
		case "admin":
			addresses = s.cfg.Ports.Admin
		}
		for _, address := range addresses {
			s.bindWG.Add(1)
			s.httpServeWG.Go(func() {
				bound := sync.OnceFunc(s.bindWG.Done)
				defer bound()
				var listeners []net.Listener
				var err error
				if name == "http" {
					listeners, err = newTCPListeners(dnsCtx, name, config.ListenConfig{address},
						s.cfg.Ports.ProxyProtocol.Has(config.ProxyProtocolTypeHttp))
				} else {
					listeners, err = newTLSListeners(dnsCtx, name, config.ListenConfig{address}, s.serverTLSConfig,
						name == httpsName && s.cfg.Ports.ProxyProtocol.Has(config.ProxyProtocolTypeHttps))
				}
				if err != nil {
					if dnsCtx.Err() == nil {
						select {
						case errCh <- err:
						case <-dnsCtx.Done():
						}
					}

					return
				}
				listener := listeners[0]
				s.httpMu.Lock()
				if dnsCtx.Err() != nil {
					s.httpMu.Unlock()
					closeAll(listeners)

					return
				}
				s.servers[listener] = srv
				s.httpMu.Unlock()
				bound()
				logger().InfoContext(ctx, fmt.Sprintf("%s server is up and running on address %s",
					strings.ToUpper(srv.String()), listener.Addr()))
				if err := srv.Serve(ctx, listener); err != nil && dnsCtx.Err() == nil {
					select {
					case errCh <- fmt.Errorf("%s on %s: %w", srv, listener.Addr(), err):
					case <-dnsCtx.Done():
					}
				}
			})
		}
	}

	if s.http3Server != nil {
		for _, pc := range s.http3PacketConns {
			s.httpServeWG.Go(func() {
				if dnsCtx.Err() != nil {
					return
				}
				logger().InfoContext(ctx, fmt.Sprintf("%s server is up and running on address %s",
					strings.ToUpper(s.http3Server.String()), pc.LocalAddr()))

				err := s.http3Server.inner.Serve(pc)
				if err != nil && dnsCtx.Err() == nil &&
					!errors.Is(err, quic.ErrServerClosed) &&
					!errors.Is(err, http.ErrServerClosed) &&
					!errors.Is(err, net.ErrClosed) {
					select {
					case errCh <- fmt.Errorf("%s on %s: %w", s.http3Server, pc.LocalAddr(), err):
					case <-dnsCtx.Done():
					}
				}
			})
		}
	}
	// Register every worker before Stop can begin waiting.
	s.dnsCancelMu.Unlock()

	registerPrintConfigurationTrigger(dnsCtx, s)
}

// Stop stops the server
//
//nolint:funlen // Preserve shutdown ordering and the upstream layout to ease future merges.
func (s *Server) Stop(ctx context.Context) error {
	logger().InfoContext(ctx, "Stopping server")
	var errs []error
	if s.stopped.Swap(true) {
		// A repeated Stop is only an error when DNS servers were configured;
		// without them there is nothing left to double-stop (and the previous
		// implementation returned nil), so keep that behaviour for callers that
		// stop a server defensively.
		if len(s.dnsServers) > 0 {
			return errors.New("server already stopped")
		}

		return nil
	}
	// Cancel every DNS startup context first: a listener whose bind is still
	// being retried has no socket to close, and cancelling the context makes
	// the retry loop and the serve loop (see retryServe) stop promptly.
	s.dnsCancelMu.Lock()
	for _, cancel := range s.dnsStartCancels {
		cancel()
	}
	s.dnsStartCancels = nil
	s.dnsCancelMu.Unlock()
	// Pending binds are cancellation-aware; wait before reading sockets or snapshotting listeners.
	s.bindWG.Wait()

	// Shut down HTTP/3 in order: server first (drains in-flight
	// requests and unblocks the Serve goroutines), then UDP packet
	// conns. Closing the packet conns first would cause Serve to
	// return a non-sentinel error that would land in errCh as a
	// spurious "server start failed".
	if s.http3Server != nil {
		if err := s.http3Server.inner.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop http3 server: %w", err))
			_ = s.http3Server.Close()
		}
	}

	for _, pc := range s.http3PacketConns {
		if err := pc.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, fmt.Errorf("close http3 packet conn: %w", err))
		}
	}

	for listener, srv := range s.servers {
		if err := srv.inner.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop %s server: %w", srv.name, err))
			_ = srv.inner.Close()
		}
		// Shutdown cannot close a listener whose Serve loop has not started yet.
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, fmt.Errorf("close %s listener: %w", srv.name, err))
		}
	}
	httpDone := make(chan struct{})
	go func() {
		s.httpServeWG.Wait()
		close(httpDone)
	}()
	select {
	case <-httpDone:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("stopping HTTP listeners timed out: %w", ctx.Err()))
	}

	// Wait for the DNS serve goroutines to finish, bounded by the shutdown
	// timeout carried by ctx. Keep enforcing cancellation when ctx is already
	// done so shutdown cannot wait indefinitely for an in-flight request.
	dnsDone := make(chan struct{})
	go func() {
		s.dnsServeWG.Wait()
		close(dnsDone)
	}()
	select {
	case <-dnsDone:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("stopping DNS listeners timed out: %w", ctx.Err()))
	}
	closeDNSServers(s.dnsServers)
	resolver.ForEach(s.queryResolver, func(res resolver.Resolver) {
		if c, ok := res.(io.Closer); ok {
			if err := c.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close %T: %w", res, err))
			}
		}
	})

	return errors.Join(errs...)
}

func extractClientIDFromHost(hostName string) string {
	const clientIDPrefix = "id-"
	if strings.HasPrefix(hostName, clientIDPrefix) && strings.Contains(hostName, ".") {
		return hostName[len(clientIDPrefix):strings.Index(hostName, ".")]
	}

	return ""
}

func newRequest(
	ctx context.Context,
	clientIP net.IP, clientID string,
	protocol model.RequestProtocol, request *dns.Msg,
) (context.Context, *model.Request) {
	ctx = log.CtxWithAttrs(ctx,
		slog.String("req_id", uuid.NewV7().String()),
		slog.Any("question", util.LogQuestions(request.Question)),
		slog.Any("client_ip", clientIP),
	)
	logger := log.FromCtx(ctx)

	logger.LogAttrs(
		ctx,
		log.LevelTrace,
		"new incoming request",
		slog.Any("client_request_id", request.Id),
		slog.Any("client_id", clientID),
		slog.Any("protocol", protocol),
	)

	req := model.Request{
		ClientIP:        clientIP,
		RequestClientID: clientID,
		Protocol:        protocol,
		Req:             request,
		RequestTS:       time.Now(),
	}

	return ctx, &req
}

func newRequestFromDNS(ctx context.Context, rw dns.ResponseWriter, msg *dns.Msg) (context.Context, *model.Request) {
	var (
		clientIP net.IP
		protocol model.RequestProtocol
	)

	if rw != nil {
		clientIP, protocol = resolveClientIPAndProtocol(rw.RemoteAddr())
	}

	var clientID string
	if con, ok := rw.(dns.ConnectionStater); ok && con.ConnectionState() != nil {
		clientID = extractClientIDFromHost(con.ConnectionState().ServerName)
	}

	return newRequest(ctx, clientIP, clientID, protocol, msg)
}

func newRequestFromHTTP(ctx context.Context, req *http.Request, msg *dns.Msg) (context.Context, *model.Request) {
	protocol := model.RequestProtocolTCP
	clientIP := util.HTTPClientIP(req)

	clientID := chi.URLParam(req, "clientID")
	if clientID == "" {
		clientID = extractClientIDFromHost(req.Host)
	}

	return newRequest(ctx, clientIP, clientID, protocol, msg)
}

// OnRequest will be executed if a new DNS request is received
func (s *Server) OnRequest(ctx context.Context, w dns.ResponseWriter, msg *dns.Msg) {
	ctx, request := newRequestFromDNS(ctx, w, msg)

	// Native DNS transports retain silent drops for rate-limited queries.
	_ = s.handleReq(ctx, request, w)
}

type msgWriter interface {
	WriteMsg(msg *dns.Msg) error
}

func (s *Server) handleReq(ctx context.Context, request *model.Request, w msgWriter) error {
	response, err := s.resolve(ctx, request)
	switch {
	case errors.Is(err, resolver.ErrRateLimited):
		return err
	case err != nil:
		log.FromCtx(ctx).ErrorContext(ctx, fmt.Sprint("error on processing request:", err))
		m := new(dns.Msg)
		m.SetRcode(request.Req, dns.RcodeServerFailure)
		err := w.WriteMsg(m)
		util.LogOnError(ctx, "can't write message: ", err)
	default:
		err := w.WriteMsg(response.Res)
		util.LogOnError(ctx, "can't write message: ", err)
	}

	return nil
}

func (s *Server) resolve(ctx context.Context, request *model.Request) (response *model.Response, rerr error) {
	defer func() {
		if val := recover(); val != nil {
			rerr = fmt.Errorf("panic occurred: %v", val)
		}
	}()

	contextUpstreamTimeoutMultiplier := 100
	timeoutDuration := time.Duration(contextUpstreamTimeoutMultiplier) * s.cfg.Upstreams.Timeout.ToDuration()

	ctx, cancel := context.WithTimeout(ctx, timeoutDuration)

	defer cancel()

	// The resolver chain mutates request.Req in place, so capture what the client itself asked for
	// up front: the response is normalized against that, not the mutated request.
	query := newClientQuery(request)

	// Denyzer doesn't implement DNS Cookies (RFC 7873), so a Server Cookie a client returns is one
	// an upstream issued and denyzer can neither validate nor reissue it. Forwarding it is worse
	// than dropping it: it is likely to reach a different upstream than the one that issued it
	// (`parallel_best` picks at random), which can't validate it either and may answer BADCOOKIE.
	// The OPT record itself is kept, since it still carries the DO bit and the buffer size the
	// client advertised.
	util.RemoveEdns0OptionKeepRecord[*dns.EDNS0_COOKIE](request.Req)

	switch {
	case len(request.Req.Question) == 0:
		m := new(dns.Msg)
		m.SetRcode(request.Req, dns.RcodeFormatError)

		log.FromCtx(ctx).ErrorContext(ctx, "query has no questions")

		response = &model.Response{Res: m, RType: model.ResponseTypeCUSTOMDNS, Reason: "CUSTOM DNS"}
	default:
		var err error

		response, err = s.queryResolver.Resolve(ctx, request)
		if err != nil {
			if upstreamErr, ok := errors.AsType[*resolver.UpstreamServerError](err); ok {
				response = &model.Response{Res: upstreamErr.Msg, RType: model.ResponseTypeRESOLVED, Reason: upstreamErr.Error()}
			} else {
				return nil, fmt.Errorf("query resolution failed: %w", err)
			}
		}
	}

	query.normalizeResponse(response.Res)

	return response, nil
}

// OnHealthCheck Handler for docker health check. Just returns OK code without delegating to resolver chain
func (s *Server) OnHealthCheck(ctx context.Context, w dns.ResponseWriter, request *dns.Msg) {
	resp := new(dns.Msg)
	resp.SetReply(request)
	resp.Rcode = dns.RcodeSuccess

	err := w.WriteMsg(resp)
	util.LogOnError(ctx, "can't write message: ", err)
}

func resolveClientIPAndProtocol(addr net.Addr) (ip net.IP, protocol model.RequestProtocol) {
	switch a := addr.(type) {
	case *net.UDPAddr:
		return a.IP, model.RequestProtocolUDP
	case *net.TCPAddr:
		return a.IP, model.RequestProtocolTCP
	}

	return nil, model.RequestProtocolUDP
}
