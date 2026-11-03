package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mia0x75/denyzer/auth"
	"github.com/mia0x75/denyzer/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const authTestPassword = "test-only-login-secret"

type authTestLogin struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func authTestHandler(a *authService) http.Handler {
	router := chi.NewRouter()
	router.Get("/api/login", a.loginStatus)
	router.Post("/api/login", a.login)
	router.Post("/api/logout", a.logout)
	router.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	return withCommonMiddleware(a.middleware(router))
}

func authTestRequest(handler http.Handler, method, target, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequestWithContext(
		context.Background(),
		method,
		target,
		nil,
	)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	return recorder
}

func authTestSignIn(t *testing.T, handler http.Handler, password string) (authTestLogin, *http.Cookie) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": "admin", "password": password})
	require.NoError(t, err)
	request := httptest.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"/api/login",
		strings.NewReader(string(body)),
	)
	request.Header.Set("Content-Type", jsonContentType)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var result authTestLogin
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &result))
	response := http.Response{Header: recorder.Header()}
	cookies := response.Cookies()
	require.Len(t, cookies, 1)

	return result, cookies[0]
}

//nolint:gocognit // Keep the authentication security cases and their shared password hash fixture together.
func TestAuthService(t *testing.T) {
	hash, err := auth.HashPassword([]byte(authTestPassword))
	require.NoError(t, err)
	credentials := config.Auth{Username: "admin", PasswordHash: hash}

	t.Run("login and Bearer versus page cookie", func(t *testing.T) {
		service := newAuthService(&config.Config{Auth: credentials})
		handler := authTestHandler(service)
		status := authTestRequest(handler, http.MethodGet, "/api/login", "")
		assert.JSONEq(t, `{"enabled":true}`, status.Body.String())
		assert.Equal(t, "no-store", status.Header().Get("Cache-Control"))
		before := time.Now()
		login, cookie := authTestSignIn(t, handler, authTestPassword)
		random, err := base64.RawURLEncoding.DecodeString(login.Token)
		require.NoError(t, err)
		assert.Len(t, random, sessionTokenSize)
		assert.WithinDuration(t, before.Add(7*24*time.Hour), login.ExpiresAt, 5*time.Second)
		assert.Equal(t, pageCookieName, cookie.Name)
		assert.Equal(t, login.Token, cookie.Value)
		assert.Equal(t, "/", cookie.Path)
		assert.True(t, cookie.HttpOnly)
		assert.Equal(t, http.SameSiteStrictMode, cookie.SameSite)
		assert.True(t, cookie.Secure)
		assert.Equal(t, int(sessionTTL.Seconds()), cookie.MaxAge,
			"the page cookie must outlive a browser restart for the session lifetime")
		for _, path := range []string{"/", "/docs/config.schema.json", "/api/status", "/debug/pprof", "/metrics"} {
			t.Run(path, func(t *testing.T) {
				assert.Equal(t, http.StatusOK, authTestRequest(handler, http.MethodGet, path, login.Token).Code)
				request := httptest.NewRequestWithContext(
					t.Context(),
					http.MethodGet,
					path,
					nil,
				)
				request.AddCookie(cookie)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if isManagementAPI(path) {
					assert.Equal(t, http.StatusUnauthorized, recorder.Code, "API and debug must never accept cookies")
				} else {
					assert.Equal(t, http.StatusOK, recorder.Code)
				}
			})
		}
	})

	t.Run("status hands the page session to a new tab", func(t *testing.T) {
		handler := authTestHandler(newAuthService(&config.Config{Auth: credentials}))
		login, cookie := authTestSignIn(t, handler, authTestPassword)

		status := func(cookie *http.Cookie) *httptest.ResponseRecorder {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/login", nil)
			if cookie != nil {
				request.AddCookie(cookie)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			return recorder
		}

		assert.JSONEq(t, `{"enabled":true,"token":"`+login.Token+`"}`, status(cookie).Body.String())

		forged := *cookie
		forged.Value = "not-a-session-token"
		assert.JSONEq(t, `{"enabled":true}`, status(&forged).Body.String())
		assert.JSONEq(t, `{"enabled":true}`, status(nil).Body.String())

		require.Equal(t, http.StatusNoContent,
			authTestRequest(handler, http.MethodPost, "/api/logout", login.Token).Code)
		assert.JSONEq(t, `{"enabled":true}`, status(cookie).Body.String(),
			"a revoked session must not be handed to a new tab")
	})

	t.Run("anonymous whitelist and traversal", func(t *testing.T) {
		handler := authTestHandler(newAuthService(&config.Config{Auth: credentials}))
		for _, tc := range []struct {
			method string
			path   string
			status int
		}{
			{method: http.MethodGet, path: "/login", status: http.StatusOK},
			{method: http.MethodHead, path: "/login/", status: http.StatusOK},
			{method: http.MethodGet, path: "/login/index.html", status: http.StatusOK},
			{method: http.MethodGet, path: "/icon", status: http.StatusOK},
			{method: http.MethodGet, path: "/_next/static/chunks/login.js", status: http.StatusOK},
			{method: http.MethodGet, path: "/", status: http.StatusSeeOther},
			{method: http.MethodHead, path: "/", status: http.StatusSeeOther},
			{method: http.MethodGet, path: "/docs/openapi.yaml", status: http.StatusSeeOther},
			{method: http.MethodGet, path: "/_next/static/../../index.html", status: http.StatusSeeOther},
			{method: http.MethodGet, path: "/_next/static/%2e%2e/%2e%2e/index.html", status: http.StatusSeeOther},
			{method: http.MethodGet, path: "/login/../index.html", status: http.StatusSeeOther},
			{method: http.MethodGet, path: "/api/status", status: http.StatusUnauthorized},
			{method: http.MethodGet, path: "/debug/pprof", status: http.StatusUnauthorized},
			{method: http.MethodPost, path: "/api/logout", status: http.StatusUnauthorized},
			{method: http.MethodPost, path: "/login/", status: http.StatusUnauthorized},
			{method: http.MethodDelete, path: "/api/login", status: http.StatusUnauthorized},
		} {
			t.Run(tc.method+" "+tc.path, func(t *testing.T) {
				result := authTestRequest(handler, tc.method, tc.path, "")
				assert.Equal(t, tc.status, result.Code)
				if tc.status == http.StatusSeeOther {
					assert.Equal(t, "/login/", result.Header().Get("Location"))
				}
				if tc.status == http.StatusUnauthorized {
					assert.Equal(t, "invalid credentials\n", result.Body.String())
					assert.Equal(t, "Bearer", result.Header().Get("WWW-Authenticate"))
				}
			})
		}
	})

	t.Run("uniform credential failures and bounded login input", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			body        string
			contentType string
			status      int
		}{
			{name: "wrong username", body: `{"username":"other","password":"` + authTestPassword + `"}`, status: http.StatusUnauthorized},
			{name: "wrong password", body: `{"username":"admin","password":"wrong"}`, status: http.StatusUnauthorized},
			{name: "empty password", body: `{"username":"admin","password":""}`, status: http.StatusUnauthorized},
			{name: "missing credentials", body: `{}`, status: http.StatusUnauthorized},
			{name: "oversized password", body: `{"username":"admin","password":"` + strings.Repeat("x", auth.MaxPasswordBytes+1) + `"}`, status: http.StatusUnauthorized},
			{name: "invalid JSON", body: `{`, status: http.StatusBadRequest},
			{name: "unknown field", body: `{"username":"admin","password":"x","admin":true}`, status: http.StatusBadRequest},
			{name: "multiple JSON values", body: `{} {}`, status: http.StatusBadRequest},
			{name: "oversized request", body: strings.Repeat(" ", loginBodyLimit) + `{}`, status: http.StatusBadRequest},
			{name: "form submission", body: "username=admin&password=secret", contentType: "application/x-www-form-urlencoded", status: http.StatusUnsupportedMediaType},
		} {
			t.Run(tc.name, func(t *testing.T) {
				handler := authTestHandler(newAuthService(&config.Config{Auth: credentials}))
				request := httptest.NewRequestWithContext(
					t.Context(),
					http.MethodPost,
					"/api/login",
					strings.NewReader(tc.body),
				)
				contentType := jsonContentType
				if tc.contentType != "" {
					contentType = tc.contentType
				}
				request.Header.Set("Content-Type", contentType)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				assert.Equal(t, tc.status, recorder.Code)
				assert.Empty(t, recorder.Header().Get("Set-Cookie"))
				if tc.status == http.StatusUnauthorized {
					assert.Equal(t, "invalid credentials\n", recorder.Body.String())
				}
			})
		}
	})

	t.Run("Bearer parsing and no query token", func(t *testing.T) {
		handler := authTestHandler(newAuthService(&config.Config{Auth: credentials}))
		login, _ := authTestSignIn(t, handler, authTestPassword)
		for _, tc := range []struct {
			name    string
			headers []string
			status  int
		}{
			{name: "case insensitive scheme", headers: []string{"bearer " + login.Token}, status: http.StatusOK},
			{name: "missing header", status: http.StatusUnauthorized},
			{name: "duplicate header", headers: []string{"Bearer " + login.Token, "Bearer " + login.Token}, status: http.StatusUnauthorized},
			{name: "extra whitespace", headers: []string{"Bearer  " + login.Token}, status: http.StatusUnauthorized},
			{name: "other scheme", headers: []string{"Basic " + login.Token}, status: http.StatusUnauthorized},
			{name: "unknown token", headers: []string{"Bearer " + strings.Repeat("x", len(login.Token))}, status: http.StatusUnauthorized},
		} {
			t.Run(tc.name, func(t *testing.T) {
				request := httptest.NewRequestWithContext(
					t.Context(),
					http.MethodGet,
					"/api/status?token="+login.Token,
					nil,
				)
				for _, header := range tc.headers {
					request.Header.Add("Authorization", header)
				}
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				assert.Equal(t, tc.status, recorder.Code)
			})
		}
	})

	t.Run("logout and restart revoke the same navigation session", func(t *testing.T) {
		cfg := &config.Config{Auth: credentials}
		handler := authTestHandler(newAuthService(cfg))
		first, cookie := authTestSignIn(t, handler, authTestPassword)
		second, _ := authTestSignIn(t, handler, authTestPassword)
		assert.NotEqual(t, first.Token, second.Token)
		result := authTestRequest(handler, http.MethodPost, "/api/logout", first.Token)
		assert.Equal(t, http.StatusNoContent, result.Code)
		response := http.Response{Header: result.Header()}
		cookies := response.Cookies()
		require.Len(t, cookies, 1)
		assert.Equal(t, -1, cookies[0].MaxAge)
		assert.Equal(t, http.StatusUnauthorized, authTestRequest(handler, http.MethodGet, "/api/status", first.Token).Code)
		assert.Equal(t, http.StatusOK, authTestRequest(handler, http.MethodGet, "/api/status", second.Token).Code)
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			"/",
			nil,
		)
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusSeeOther, recorder.Code)
		restarted := authTestHandler(newAuthService(cfg))
		assert.Equal(t, http.StatusUnauthorized, authTestRequest(restarted, http.MethodGet, "/api/status", second.Token).Code)
	})

	t.Run("fixed seven day expiry never slides", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			handler := authTestHandler(newAuthService(&config.Config{Auth: credentials}))
			login, cookie := authTestSignIn(t, handler, authTestPassword)
			time.Sleep(24 * time.Hour)
			assert.Equal(t, http.StatusOK, authTestRequest(handler, http.MethodGet, "/api/status", login.Token).Code)
			time.Sleep(time.Until(login.ExpiresAt) - time.Nanosecond)
			assert.Equal(t, http.StatusOK, authTestRequest(handler, http.MethodGet, "/api/status", login.Token).Code)
			time.Sleep(time.Nanosecond)
			assert.Equal(t, http.StatusUnauthorized, authTestRequest(handler, http.MethodGet, "/api/status", login.Token).Code)
			request := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodGet,
				"/",
				nil,
			)
			request.AddCookie(cookie)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			assert.Equal(t, http.StatusSeeOther, recorder.Code)
		})
	})

	t.Run("disk password reset revokes without restarting", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yml")
		require.NoError(t, config.SetAuth(path, credentials))
		cfg, err := config.LoadConfig(path, true)
		require.NoError(t, err)
		handler := authTestHandler(newAuthService(cfg))
		login, cookie := authTestSignIn(t, handler, authTestPassword)
		newHash, err := auth.HashPassword([]byte("replacement-secret"))
		require.NoError(t, err)
		require.NoError(t, config.SetAuth(path, config.Auth{Username: "admin", PasswordHash: newHash}))
		assert.Equal(t, http.StatusUnauthorized, authTestRequest(handler, http.MethodGet, "/api/status", login.Token).Code)
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodGet,
			"/",
			nil,
		)
		request.AddCookie(cookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusSeeOther, recorder.Code)
		body := `{"username":"admin","password":"` + authTestPassword + `"}`
		request = httptest.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			"/api/login",
			strings.NewReader(body),
		)
		request.Header.Set("Content-Type", jsonContentType)
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		fresh, _ := authTestSignIn(t, handler, "replacement-secret")
		assert.Equal(t, http.StatusOK, authTestRequest(handler, http.MethodGet, "/api/status", fresh.Token).Code)
	})

	t.Run("credential read failures fail closed and never revive sessions", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			data   string
			remove bool
		}{
			{name: "missing file", remove: true},
			{name: "malformed YAML", data: "auth: [\n"},
			{name: "unknown auth field", data: "auth: {password: plaintext}\n"},
			{name: "duplicate auth", data: "auth: {}\nauth: {}\n"},
			{name: "multiple documents", data: "auth: {}\n---\nauth: {}\n"},
			{name: "disabled while running", data: "ports: {admin: '127.0.0.1:4000'}\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yml")
				require.NoError(t, config.SetAuth(path, credentials))
				cfg, err := config.LoadConfig(path, true)
				require.NoError(t, err)
				handler := authTestHandler(newAuthService(cfg))
				login, _ := authTestSignIn(t, handler, authTestPassword)
				if tc.remove {
					require.NoError(t, os.Remove(path))
				} else {
					require.NoError(t, os.WriteFile(path, []byte(tc.data), 0o600))
				}
				assert.Equal(t, http.StatusServiceUnavailable, authTestRequest(handler, http.MethodGet, "/api/status", login.Token).Code)
				assert.Equal(t, http.StatusServiceUnavailable, authTestRequest(handler, http.MethodGet, "/api/login", "").Code)
				// Restore a valid file externally; failed reads must permanently revoke the old session.
				require.NoError(t, os.WriteFile(path, nil, 0o600))
				require.NoError(t, config.SetAuth(path, credentials))
				assert.Equal(t, http.StatusUnauthorized, authTestRequest(handler, http.MethodGet, "/api/status", login.Token).Code)
			})
		}
	})

	t.Run("reset during password verification cannot mint an old session", func(t *testing.T) {
		service := newAuthService(&config.Config{Auth: credentials})
		reads := 0
		service.load = func() (config.Auth, error) {
			reads++
			current := credentials
			if reads > 1 {
				current.Username = "replacement-admin"
			}

			return current, nil
		}
		body := `{"username":"admin","password":"` + authTestPassword + `"}`
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			"/api/login",
			strings.NewReader(body),
		)
		request.Header.Set("Content-Type", jsonContentType)
		recorder := httptest.NewRecorder()
		authTestHandler(service).ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		assert.Empty(t, recorder.Header().Get("Set-Cookie"))
		assert.Equal(t, 2, reads)
	})

	t.Run("disabled loopback authentication can be enabled live", func(t *testing.T) {
		cfg := &config.Config{}
		handler := authTestHandler(newAuthService(cfg))
		assert.JSONEq(t, `{"enabled":false}`, authTestRequest(handler, http.MethodGet, "/api/login", "").Body.String())
		assert.Equal(t, http.StatusOK, authTestRequest(handler, http.MethodGet, "/api/status", "").Code)
		cfg.Auth = credentials
		assert.Equal(t, http.StatusUnauthorized, authTestRequest(handler, http.MethodGet, "/api/status", "").Code)
		assert.JSONEq(t, `{"enabled":true}`, authTestRequest(handler, http.MethodGet, "/api/login", "").Body.String())
	})

	t.Run("login rate limit is per client", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			handler := authTestHandler(newAuthService(&config.Config{Auth: credentials}))
			attempt := func(peer string) *httptest.ResponseRecorder {
				request := httptest.NewRequestWithContext(
					t.Context(),
					http.MethodPost,
					"/api/login",
					strings.NewReader(`{}`),
				)
				request.Header.Set("Content-Type", jsonContentType)
				request.RemoteAddr = peer
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)

				return recorder
			}
			for i := range loginBurst {
				// A new source port per attempt must still share the same budget.
				assert.Equal(t, http.StatusUnauthorized, attempt(fmt.Sprintf("192.0.2.1:%d", 4000+i)).Code)
			}
			limited := attempt("192.0.2.1:4999")
			assert.Equal(t, http.StatusTooManyRequests, limited.Code)
			assert.Equal(t, "1", limited.Header().Get("Retry-After"))
			// Another client keeps its own budget, so one host cannot lock the administrator out.
			assert.Equal(t, http.StatusUnauthorized, attempt("192.0.2.2:1234").Code)
			time.Sleep(time.Second)
			assert.Equal(t, http.StatusUnauthorized, attempt("192.0.2.1:1234").Code)
		})
	})

	t.Run("only two concurrent login operations", func(t *testing.T) {
		service := newAuthService(&config.Config{Auth: credentials})
		handler := authTestHandler(service)
		service.loginSlots <- struct{}{}
		service.loginSlots <- struct{}{}
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			"/api/login",
			strings.NewReader(`{}`),
		)
		request.Header.Set("Content-Type", jsonContentType)
		rejected := httptest.NewRecorder()
		handler.ServeHTTP(rejected, request)
		assert.Equal(t, http.StatusTooManyRequests, rejected.Code)
		<-service.loginSlots
		<-service.loginSlots

		recorders := []*httptest.ResponseRecorder{httptest.NewRecorder(), httptest.NewRecorder()}
		var group sync.WaitGroup
		for _, recorder := range recorders {
			group.Go(func() {
				body := `{"username":"admin","password":"` + authTestPassword + `"}`
				request := httptest.NewRequestWithContext(
					t.Context(),
					http.MethodPost,
					"/api/login",
					strings.NewReader(body),
				)
				request.Header.Set("Content-Type", jsonContentType)
				handler.ServeHTTP(recorder, request)
			})
		}
		group.Wait()
		tokens := make([]string, 0, len(recorders))
		for _, recorder := range recorders {
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			var login authTestLogin
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &login))
			tokens = append(tokens, login.Token)
		}
		assert.NotEqual(t, tokens[0], tokens[1])
	})

	t.Run("bounded sessions and expiry cleanup", func(t *testing.T) {
		service := newAuthService(&config.Config{Auth: credentials})
		for i := range maxSessions {
			service.sessions[sha256.Sum256([]byte{byte(i)})] = time.Now().Add(time.Hour)
		}
		handler := authTestHandler(service)
		body := `{"username":"admin","password":"` + authTestPassword + `"}`
		request := httptest.NewRequestWithContext(
			t.Context(),
			http.MethodPost,
			"/api/login",
			strings.NewReader(body),
		)
		request.Header.Set("Content-Type", jsonContentType)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusTooManyRequests, recorder.Code)
		assert.Empty(t, recorder.Header().Get("Set-Cookie"))
		service.sessions[sha256.Sum256([]byte{0})] = time.Now().Add(-time.Second)
		login, _ := authTestSignIn(t, handler, authTestPassword)
		assert.Equal(t, http.StatusOK, authTestRequest(handler, http.MethodGet, "/api/status", login.Token).Code)
	})
}

func TestManagementOrigin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		origin    string
		target    string
		peer      string
		forwarded string
		fetchSite string
		allowed   bool
	}{
		{name: "CLI without Origin", allowed: true},
		{name: "same origin", origin: "https://admin.test:4000", allowed: true},
		{name: "other host", origin: "https://attacker.test:4000"},
		{name: "other port", origin: "https://admin.test:4001"},
		{name: "other scheme", origin: "http://admin.test:4000"},
		{name: "null origin", origin: "null"},
		{name: "userinfo", origin: "https://user@admin.test:4000"},
		{name: "origin with path", origin: "https://admin.test:4000/path"},
		{name: "cross site navigation", fetchSite: "cross-site"},
		{name: "proxy header cannot allow HTTP origin", origin: "http://admin.test:4000", peer: "127.0.0.1:1234", forwarded: "http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target := "https://admin.test:4000/api/status"
			if tc.target != "" {
				target = tc.target
			}
			request := httptest.NewRequestWithContext(
				t.Context(),
				http.MethodGet,
				target,
				nil,
			)
			request.Header.Set("Origin", tc.origin)
			request.Header.Set("X-Forwarded-Proto", tc.forwarded)
			request.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			if tc.peer != "" {
				request.RemoteAddr = tc.peer
			}
			recorder := httptest.NewRecorder()
			authTestHandler(newAuthService(&config.Config{})).ServeHTTP(recorder, request)
			status := http.StatusForbidden
			if tc.allowed {
				status = http.StatusOK
			}
			assert.Equal(t, status, recorder.Code)
			assert.Empty(t, recorder.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			assert.Contains(t, recorder.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'")
			cookieRecorder := httptest.NewRecorder()
			setPageCookie(cookieRecorder, "test-token")
			response := http.Response{Header: cookieRecorder.Header()}
			cookies := response.Cookies()
			require.Len(t, cookies, 1)
			assert.True(t, cookies[0].Secure)
		})
	}
}

func TestLoginLimitersBounded(t *testing.T) {
	t.Parallel()

	limiters := &newAuthService(&config.Config{}).loginLimit
	for i := range maxLoginClients + 10 {
		require.True(t, limiters.allow(fmt.Sprintf("192.0.2.%d", i)))
	}
	assert.LessOrEqual(t, len(limiters.buckets), maxLoginClients)
	// A fresh client is still served when the table is full; only old buckets are dropped.
	assert.True(t, limiters.allow("198.51.100.1"))
}
