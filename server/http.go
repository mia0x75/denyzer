package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mia0x75/denyzer/config"
	"github.com/rs/cors"
)

type httpServer struct {
	inner http.Server

	name string
}

func newHTTPServer(name string, handler http.Handler, cfg *config.Config) *httpServer {
	var (
		writeTimeout      = cfg.Blocking.Loading.Downloads.WriteTimeout
		readTimeout       = cfg.Blocking.Loading.Downloads.ReadTimeout
		readHeaderTimeout = cfg.Blocking.Loading.Downloads.ReadHeaderTimeout
	)
	errorHandler := &httpErrorLogHandler{
		Handler:    logger().With("listener", name).Handler(),
		selfSigned: cfg.CertFile == "" && cfg.KeyFile == "",
	}

	return &httpServer{
		inner: http.Server{
			ErrorLog:          slog.NewLogLogger(errorHandler, slog.LevelWarn),
			ReadTimeout:       time.Duration(readTimeout),
			ReadHeaderTimeout: time.Duration(readHeaderTimeout),
			WriteTimeout:      time.Duration(writeTimeout),
			Handler:           withCommonMiddleware(handler),
		},

		name: name,
	}
}

type httpErrorLogHandler struct {
	slog.Handler

	selfSigned bool
}

func (h *httpErrorLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if h.selfSigned && strings.HasPrefix(record.Message, "http: TLS handshake error") {
		// The startup warning already explains why clients reject the generated certificate.
		switch {
		case strings.HasSuffix(record.Message, "remote error: tls: unknown certificate"),
			strings.HasSuffix(record.Message, "remote error: tls: bad certificate"),
			strings.HasSuffix(record.Message, "remote error: tls: unknown certificate authority"):
			record.Level = slog.LevelDebug
		}
	}
	if !h.Enabled(ctx, record.Level) {
		return nil
	}

	return h.Handler.Handle(ctx, record)
}

func (s *httpServer) String() string {
	return s.name
}

func (s *httpServer) Serve(ctx context.Context, l net.Listener) error {
	go func() {
		<-ctx.Done()

		s.inner.Close()
	}()

	if err := s.inner.Serve(l); err != nil {
		return fmt.Errorf("HTTP server '%s' failed to serve: %w", s.name, err)
	}

	return nil
}

func withCommonMiddleware(inner http.Handler) *chi.Mux {
	// Middleware must be defined before routes, so
	// create a new router and mount the inner handler
	mux := chi.NewMux()

	mux.Use(
		secureHeadersMiddleware,
	)

	mux.Mount("/", inner)

	return mux
}

type httpMiddleware = func(http.Handler) http.Handler

func secureHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000")
			w.Header().Set("X-XSS-Protection", "1; mode=block")
		}

		next.ServeHTTP(w, r)
	})
}

func newCORSMiddleware() httpMiddleware {
	const corsMaxAge = 5 * time.Minute

	options := cors.Options{
		// DoH is public; management routes never use this middleware.
		AllowedHeaders: []string{"Content-Type"},
		AllowedMethods: []string{"GET", "POST"},
		AllowedOrigins: []string{"*"},
		// Allow Chromium's Private Network Access preflights, sent when a
		// public site addresses a private DoH endpoint.
		AllowPrivateNetwork: true,
		ExposedHeaders:      []string{"Link"},
		MaxAge:              int(corsMaxAge.Seconds()),
	}

	return cors.New(options).Handler
}
