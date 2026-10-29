package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/mia0x75/denyzer/resolver"

	"github.com/mia0x75/denyzer/api"
	"github.com/mia0x75/denyzer/config"
	"github.com/mia0x75/denyzer/docs"
	"github.com/mia0x75/denyzer/log"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/util"
	"github.com/mia0x75/denyzer/web"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/miekg/dns"
)

const (
	dohMessageLimit    = 512
	contentTypeHeader  = "Content-Type"
	cacheControlHeader = "Cache-Control"
	dnsContentType     = "application/dns-message"
	htmlContentType    = "text/html; charset=UTF-8"
	yamlContentType    = "text/yaml"
	jsonContentType    = "application/json"
)

func (s *Server) createOpenAPIInterfaceImpl() (impl api.StrictServerInterface, err error) {
	bControl, err := resolver.GetFromChainWithType[api.BlockingControl](s.queryResolver)
	if err != nil {
		return nil, fmt.Errorf("no blocking API implementation found %w", err)
	}

	refresher, err := resolver.GetFromChainWithType[api.ListRefresher](s.queryResolver)
	if err != nil {
		return nil, fmt.Errorf("no refresh API implementation found %w", err)
	}

	cacheControl, err := resolver.GetFromChainWithType[api.CacheControl](s.queryResolver)
	if err != nil {
		return nil, fmt.Errorf("no cache API implementation found %w", err)
	}

	// Statistics are optional: if no provider is in the chain, degrade to a nil
	// provider (the /api/stats endpoint returns 503) instead of failing
	// construction of the entire API.
	statsProvider, err := resolver.GetFromChainWithType[api.StatsProvider](s.queryResolver)
	if err != nil {
		log.Log().Warnf("no stats API implementation found, /api/stats will be unavailable: %v", err)

		statsProvider = nil
	}

	return api.NewOpenAPIInterfaceImpl(bControl, s, refresher, cacheControl, statsProvider), nil
}

// cacheEntryCount reports the current size of the in-memory result cache
// (0 when no caching resolver is present in the chain).
func (s *Server) cacheEntryCount() int {
	caching, err := resolver.GetFromChainWithType[*resolver.CachingResolver](s.queryResolver)
	if err != nil {
		return 0
	}

	return caching.EntryCount()
}

func (s *Server) registerDoHEndpoints(router *chi.Mux, cfg *config.Config) {
	pathDohQuery := cfg.Ports.DOHPath

	router.Get(pathDohQuery, s.dohGetRequestHandler)
	router.Get(pathDohQuery+"/", s.dohGetRequestHandler)
	router.Get(pathDohQuery+"/{clientID}", s.dohGetRequestHandler)
	router.Post(pathDohQuery, s.dohPostRequestHandler)
	router.Post(pathDohQuery+"/", s.dohPostRequestHandler)
	router.Post(pathDohQuery+"/{clientID}", s.dohPostRequestHandler)
}

func (s *Server) dohGetRequestHandler(rw http.ResponseWriter, req *http.Request) {
	dnsParam, ok := req.URL.Query()["dns"]
	if !ok || len(dnsParam[0]) < 1 {
		http.Error(rw, "dns param is missing", http.StatusBadRequest)

		return
	}

	if len(dnsParam[0]) > base64.RawURLEncoding.EncodedLen(dohMessageLimit) {
		http.Error(rw, "URI Too Long", http.StatusRequestURITooLong)

		return
	}

	rawMsg, err := base64.RawURLEncoding.DecodeString(dnsParam[0])
	if err != nil {
		http.Error(rw, "wrong message format", http.StatusBadRequest)

		return
	}

	if len(rawMsg) > dohMessageLimit {
		http.Error(rw, "URI Too Long", http.StatusRequestURITooLong)

		return
	}

	s.processDohMessage(rawMsg, rw, req)
}

func (s *Server) dohPostRequestHandler(rw http.ResponseWriter, req *http.Request) {
	contentType := req.Header.Get(contentTypeHeader)
	if contentType != dnsContentType {
		http.Error(rw, "unsupported content type", http.StatusUnsupportedMediaType)

		return
	}

	rawMsg, err := io.ReadAll(io.LimitReader(req.Body, int64(dohMessageLimit)+1))
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)

		return
	}

	if len(rawMsg) > dohMessageLimit {
		http.Error(rw, "Payload Too Large", http.StatusRequestEntityTooLarge)

		return
	}

	s.processDohMessage(rawMsg, rw, req)
}

func (s *Server) processDohMessage(rawMsg []byte, rw http.ResponseWriter, httpReq *http.Request) {
	msg := new(dns.Msg)
	if err := msg.Unpack(rawMsg); err != nil {
		logger().Error("can't deserialize message: ", err)
		http.Error(rw, err.Error(), http.StatusBadRequest)

		return
	}

	ctx, dnsReq := newRequestFromHTTP(httpReq.Context(), httpReq, msg)

	s.handleReq(ctx, dnsReq, httpMsgWriter{rw})
}

type httpMsgWriter struct {
	rw http.ResponseWriter
}

func (r httpMsgWriter) WriteMsg(msg *dns.Msg) error {
	b, err := msg.Pack()
	if err != nil {
		return fmt.Errorf("failed to pack DNS message for DoH response: %w", err)
	}

	r.rw.Header().Set(contentTypeHeader, dnsContentType)

	// https://www.rfc-editor.org/rfc/rfc8484#section-5.1
	// get the smallest TTL from answer
	r.rw.Header().Set(cacheControlHeader, fmt.Sprintf("max-age=%d", getSmallestTTLFromAnswer(msg)))

	// https://www.rfc-editor.org/rfc/rfc8484#section-4.2.1
	r.rw.WriteHeader(http.StatusOK)

	_, err = r.rw.Write(b)
	if err != nil {
		return fmt.Errorf("failed to write DoH response: %w", err)
	}

	return nil
}

func getSmallestTTLFromAnswer(msg *dns.Msg) uint32 {
	var ttl uint32 = 0
	for _, a := range msg.Answer {
		if a.Header().Ttl < ttl || ttl == 0 {
			ttl = a.Header().Ttl
		}
	}

	return ttl
}

func (s *Server) Query(
	ctx context.Context, serverHost string, clientIP net.IP, question string, qType dns.Type,
) (*model.Response, error) {
	msg := util.NewMsgWithQuestion(question, qType)
	clientID := extractClientIDFromHost(serverHost)

	ctx, req := newRequest(ctx, clientIP, clientID, model.RequestProtocolTCP, msg)

	return s.resolve(ctx, req)
}

func createHTTPRouter(
	openAPIImpl api.StrictServerInterface,
	listCounts listCountsProvider,
	cacheEntryCount func() int,
) *chi.Mux {
	router := chi.NewRouter()

	api.RegisterOpenAPIEndpoints(router, openAPIImpl)

	// SQLite JSON API endpoints for the denyzer-ui dashboard.
	router.Get("/api/stats/summary", func(w http.ResponseWriter, r *http.Request) {
		handleStatsSummary(w, r, cacheEntryCount)
	})
	router.Get("/api/stats/hourly_trend", handleHourlyTrend)
	router.Get("/api/stats/top_domains", handleTopDomains)
	router.Get("/api/stats/top_clients", handleTopClients)
	router.Get("/api/stats/search_domains", handleSearchDomains)
	router.Get("/api/stats/search_clients", handleSearchClients)
	router.Get("/api/stats/query_types", handleQueryTypesBreakdown)
	router.Get("/api/logs/query", handleQueryLogs)
	router.Post("/api/stats/refresh", handleRefreshStats)
	router.Post("/api/stats/cleanup", handleCleanupHistory)
	registerStatsListsHandler(router, listCounts)

	configureDebugHandler(router)

	configureDocsHandler(router)

	configureStaticAssetsHandler(router)

	configureDashboardHandler(router)

	configureRootHandler(router)

	configureRobotsHandler(router)

	return router
}

func configureDocsHandler(router *chi.Mux) {
	router.Get("/docs/openapi.yaml", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set(contentTypeHeader, yamlContentType)
		_, err := writer.Write([]byte(docs.OpenAPI))
		logAndResponseWithError(err, "can't write OpenAPI definition file: ", writer)
	})

	router.Get("/docs/config.schema.json", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set(contentTypeHeader, jsonContentType)
		_, err := writer.Write(docs.ConfigSchema)
		logAndResponseWithError(err, "can't write config JSON schema file: ", writer)
	})
}

func configureStaticAssetsHandler(router *chi.Mux) {
	assets, err := web.Assets()
	util.FatalOnError("unable to load static asset files", err)

	fs := http.FileServer(http.FS(assets))
	router.Handle("/static/*", http.StripPrefix("/static/", fs))
}

func configureDashboardHandler(router *chi.Mux) {
	fsys, err := web.DashboardFS()
	util.FatalOnError("unable to load embedded dashboard files", err)

	dashboardFS := http.FileServer(http.FS(fsys))

	router.Get("/dashboard", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/dashboard/", http.StatusMovedPermanently)
	})
	router.Handle("/dashboard/*", http.StripPrefix("/dashboard", dashboardFS))
}

func configureRobotsHandler(router *chi.Mux) {
	router.Handle("/robots.txt", http.FileServer(http.FS(web.WebFs)))
}

func configureRootHandler(router *chi.Mux) {
	router.Get("/", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/dashboard/", http.StatusMovedPermanently)
	})
}

func logAndResponseWithError(err error, message string, writer http.ResponseWriter) {
	if err != nil {
		log.Log().Error(message, log.EscapeInput(err.Error()))
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

func configureDebugHandler(router *chi.Mux) {
	router.Mount("/debug", middleware.Profiler())
}

// listCountsProvider exposes the current per-group blocklist entry counts.
type listCountsProvider interface {
	ListCounts() (denylist, allowlist map[string]int)
}

// registerStatsListsHandler registers GET /api/stats/lists when a provider is
// available in the resolver chain.
func registerStatsListsHandler(router *chi.Mux, provider listCountsProvider) {
	if provider == nil {
		log.Log().Debug("no blocklist counts provider in resolver chain, /api/stats/lists will be unavailable")

		return
	}

	router.Get("/api/stats/lists", func(writer http.ResponseWriter, request *http.Request) {
		handleStatsListed(writer, provider)
	})
}
