package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/mia0x75/denyzer/auth"
	"github.com/mia0x75/denyzer/config"
	"golang.org/x/time/rate"
)

const (
	sessionTTL       = 7 * 24 * time.Hour
	sessionTokenSize = 32
	maxSessions      = 128
	loginBodyLimit   = 8 * 1024
	loginBurst       = 5
	maxLoginClients  = 1024
	pageCookieName   = "denyzer_page_session"
)

type authService struct {
	mu          sync.Mutex
	credentials config.Auth
	load        func() (config.Auth, error)
	sessions    map[[sha256.Size]byte]time.Time
	loginLimit  loginLimiters
	loginSlots  chan struct{}
}

// loginLimiters bounds login attempts per client, so one host cannot lock out the administrator.
type loginLimiters struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

func (l *loginLimiters) allow(client string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if limiter, ok := l.buckets[client]; ok {
		return limiter.Allow()
	}
	if len(l.buckets) >= maxLoginClients {
		// Evict one entry: dropping an attacker's bucket is harmless, and dropping
		// the administrator's only restores the same burst.
		for key := range l.buckets {
			delete(l.buckets, key)

			break
		}
	}
	limiter := rate.NewLimiter(rate.Every(time.Second), loginBurst)
	l.buckets[client] = limiter

	return limiter.Allow()
}

// clientKey identifies the connecting host. Forwarded headers are ignored: they are
// spoofable and the management listener has no proxy support, so a reverse proxy
// makes all clients share one budget.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

func newAuthService(cfg *config.Config) *authService {
	return &authService{
		credentials: cfg.Auth,
		load:        cfg.ReadAuth,
		sessions:    make(map[[sha256.Size]byte]time.Time),
		loginLimit:  loginLimiters{buckets: make(map[string]*rate.Limiter)},
		// A global cap bounds concurrent Argon2 hashes (64 MiB each); the per-client
		// limiter above stops one host from holding every slot.
		loginSlots: make(chan struct{}, 2),
	}
}

// currentLocked serializes credential reads with session updates so an older read cannot undo a reset.
func (a *authService) currentLocked() (config.Auth, error) {
	credentials, err := a.load()
	if err != nil {
		clear(a.sessions)

		return config.Auth{}, err
	}
	if a.credentials.IsEnabled() && !credentials.IsEnabled() {
		clear(a.sessions)

		return config.Auth{}, errors.New("authentication cannot be disabled without restarting the server")
	}
	if credentials != a.credentials {
		clear(a.sessions)
		a.credentials = credentials
	}

	return credentials, nil
}

func (a *authService) current() (config.Auth, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.currentLocked()
}

func (a *authService) validSession(token string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	credentials, err := a.currentLocked()
	if err != nil {
		return false, err
	}
	if !credentials.IsEnabled() {
		return true, nil
	}
	if len(token) != base64.RawURLEncoding.EncodedLen(sessionTokenSize) {
		return false, nil
	}

	key := sha256.Sum256([]byte(token))
	expiresAt, ok := a.sessions[key]
	if !ok || !time.Now().Before(expiresAt) {
		delete(a.sessions, key)

		return false, nil
	}

	return true, nil
}

func (a *authService) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; "+
			"style-src 'self' 'unsafe-inline'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if !sameOrigin(r) {
			http.Error(w, "cross-origin management requests are not allowed", http.StatusForbidden)

			return
		}
		if isPublicManagementPath(r) {
			next.ServeHTTP(w, r)

			return
		}

		token := bearerToken(r)
		isAPI := isManagementAPI(r.URL.Path)
		if !isAPI && token == "" {
			if cookie, err := r.Cookie(pageCookieName); err == nil {
				token = cookie.Value
			}
		}
		valid, err := a.validSession(token)
		if err != nil {
			logger().ErrorContext(r.Context(), "authentication configuration unavailable", "error", err)
			http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)

			return
		}
		if !valid {
			if !isAPI && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				clearPageCookie(w)
				http.Redirect(w, r, "/login/", http.StatusSeeOther)
			} else {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "invalid credentials", http.StatusUnauthorized)
			}

			return
		}
		next.ServeHTTP(w, r)
	})
}

func isManagementAPI(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/") ||
		path == "/debug" || strings.HasPrefix(path, "/debug/") || path == "/metrics"
}

func isPublicManagementPath(r *http.Request) bool {
	if r.URL.Path == "/api/login" {
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if path.Clean(r.URL.Path) != strings.TrimSuffix(r.URL.Path, "/") {
		return false
	}

	return r.URL.Path == "/login" || r.URL.Path == "/login/" || r.URL.Path == "/login/index.html" ||
		r.URL.Path == "/icon" || r.URL.Path == "/icon.png" || strings.HasPrefix(r.URL.Path, "/_next/static/")
}

func bearerToken(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return ""
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.ContainsAny(token, " \t\r\n") {
		return ""
	}

	return token
}

func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // CLI clients do not send Origin and still need a Bearer token.
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}

	return parsed.Scheme == "https" && parsed.Host == r.Host && parsed.User == nil &&
		parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func setPageCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name: pageCookieName, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearPageCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: pageCookieName, Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func (a *authService) loginStatus(w http.ResponseWriter, r *http.Request) {
	credentials, err := a.current()
	if err != nil {
		http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)

		return
	}
	status := struct {
		Enabled bool   `json:"enabled"`
		Token   string `json:"token,omitempty"`
	}{Enabled: credentials.IsEnabled()}
	// A page request is authenticated by the HttpOnly cookie, so a new tab or a
	// restarted browser can pick up the session the server still holds.
	if status.Enabled {
		if cookie, err := r.Cookie(pageCookieName); err == nil {
			if valid, err := a.validSession(cookie.Value); err == nil && valid {
				status.Token = cookie.Value
			}
		}
	}
	writeJSON(w, status)
}

func (a *authService) login(w http.ResponseWriter, r *http.Request) {
	if !a.loginLimit.allow(clientKey(r)) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)

		return
	}
	select {
	case a.loginSlots <- struct{}{}:
		defer func() { <-a.loginSlots }()
	default:
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)

		return
	}

	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != jsonContentType {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)

		return
	}
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, loginBodyLimit))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) {
		http.Error(w, "invalid login request", http.StatusBadRequest)

		return
	}

	credentials, err := a.current()
	if err != nil {
		http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)

		return
	}
	// Always verify the password, including for the wrong username.
	passwordOK := auth.VerifyPassword(credentials.PasswordHash, []byte(request.Password))
	if !credentials.IsEnabled() || !passwordOK || request.Username != credentials.Username {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)

		return
	}

	a.startSession(w, credentials)
}

func (a *authService) startSession(w http.ResponseWriter, credentials config.Auth) {
	a.mu.Lock()
	current, err := a.currentLocked()
	if err != nil || current != credentials {
		a.mu.Unlock()
		http.Error(w, "invalid credentials", http.StatusUnauthorized)

		return
	}
	now := time.Now()
	for key, expiresAt := range a.sessions {
		if !now.Before(expiresAt) {
			delete(a.sessions, key)
		}
	}
	if len(a.sessions) >= maxSessions {
		a.mu.Unlock()
		http.Error(w, "too many active sessions", http.StatusTooManyRequests)

		return
	}
	var random [sessionTokenSize]byte
	if _, err := rand.Read(random[:]); err != nil {
		a.mu.Unlock()
		http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)

		return
	}
	token := base64.RawURLEncoding.EncodeToString(random[:])
	expiresAt := now.Add(sessionTTL)
	a.sessions[sha256.Sum256([]byte(token))] = expiresAt
	a.mu.Unlock()
	setPageCookie(w, token)
	writeJSON(w, struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}{Token: token, ExpiresAt: expiresAt})
}

func (a *authService) logout(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	delete(a.sessions, sha256.Sum256([]byte(bearerToken(r))))
	a.mu.Unlock()
	clearPageCookie(w)
	w.WriteHeader(http.StatusNoContent)
}
