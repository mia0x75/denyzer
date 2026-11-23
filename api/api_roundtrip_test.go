package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mia0x75/denyzer/model"
	"github.com/mia0x75/denyzer/stats"
	"github.com/mia0x75/denyzer/util"
	"github.com/miekg/dns"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// routerTransport feeds the generated client's requests straight into the
// generated chi routes, without a socket or a real listener.
type routerTransport struct{ router http.Handler }

func (rt routerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	rt.router.ServeHTTP(recorder, r)

	resp := recorder.Result()
	resp.Request = r

	return resp, nil
}

// apiRoundTrip wires the generated client to the generated server handlers
// in one process, so the codegen contract stays covered. Each test gets its
// own mocks: re-registering a testify expectation for the same arguments
// does not replace the first return value.
type apiRoundTrip struct {
	client       *ClientWithResponses
	control      *MockBlockingControl
	querier      *MockQuerier
	refresher    *MockListRefresher
	cacheControl *MockCacheControl
	provider     *MockStatsProvider
}

func newAPIRoundTrip(t *testing.T) *apiRoundTrip {
	t.Helper()

	control := NewMockBlockingControl(t)
	querier := NewMockQuerier(t)
	refresher := NewMockListRefresher(t)
	cacheControl := NewMockCacheControl(t)
	provider := NewMockStatsProvider(t)

	router := chi.NewRouter()
	RegisterOpenAPIEndpoints(router, NewOpenAPIInterfaceImpl(control, querier, refresher, cacheControl, provider))

	client, err := NewClientWithResponses(
		// The generated routes live under the /api base, like the production
		// listener the CLI client talks to.
		"http://api.test/api",
		WithHTTPClient(&http.Client{Transport: routerTransport{router: router}}),
	)
	require.NoError(t, err)

	return &apiRoundTrip{
		client:       client,
		control:      control,
		querier:      querier,
		refresher:    refresher,
		cacheControl: cacheControl,
		provider:     provider,
	}
}

// TestAPIRoundTrip drives every generated client operation through the
// generated server handlers over in-process HTTP.
func TestAPIRoundTrip(t *testing.T) {
	t.Run("Query returns the resolution result", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		answer, err := util.NewMsgWithAnswer("example.com.", 123, dns.Type(dns.TypeA), "192.0.2.1")
		require.NoError(t, err)
		rt.querier.On("Query", mock.Anything, mock.Anything, mock.Anything, "example.com.", dns.Type(dns.TypeA)).
			Return(&model.Response{Res: answer, Reason: "RESOLVED (1.1.1.1)"}, nil)

		resp, err := rt.client.QueryWithResponse(t.Context(), QueryJSONRequestBody{Query: "example.com", Type: "A"})
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, resp.StatusCode())
		require.NotNil(t, resp.JSON200)
		require.Equal(t, "RESOLVED (1.1.1.1)", resp.JSON200.Reason)
		require.Equal(t, "RESOLVED", resp.JSON200.ResponseType)
		require.Equal(t, "A (192.0.2.1)", resp.JSON200.Response)
		require.Equal(t, "NOERROR", resp.JSON200.ReturnCode)
	})

	t.Run("Query rejects an unknown record type with 400", func(t *testing.T) {
		rt := newAPIRoundTrip(t)

		resp, err := rt.client.QueryWithResponse(t.Context(), QueryJSONRequestBody{Query: "example.com", Type: "NOPE"})
		require.NoError(t, err)

		require.Equal(t, http.StatusBadRequest, resp.StatusCode())
		require.Equal(t, "unknown query type 'NOPE'", string(resp.Body))
	})

	t.Run("Query surfaces an upstream error as 500", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		rt.querier.On("Query", mock.Anything, mock.Anything, mock.Anything, "broken.", dns.Type(dns.TypeA)).
			Return(nil, errors.New("upstream down"))

		resp, err := rt.client.QueryWithResponse(t.Context(), QueryJSONRequestBody{Query: "broken", Type: "A"})
		require.NoError(t, err)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode())
	})

	t.Run("BlockingStatus reflects the control state", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		groups := []string{"g1"}
		seconds := 30
		rt.control.On("BlockingStatus").Return(BlockingStatus{
			Enabled:         false,
			DisabledGroups:  groups,
			AutoEnableInSec: seconds,
		})

		resp, err := rt.client.BlockingStatusWithResponse(t.Context())
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, resp.StatusCode())
		require.NotNil(t, resp.JSON200)
		require.False(t, resp.JSON200.Enabled)
		require.Equal(t, groups, *resp.JSON200.DisabledGroups)
		require.Equal(t, &seconds, resp.JSON200.AutoEnableInSec)
	})

	t.Run("DisableBlocking parses and forwards its parameters", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		rt.control.On("DisableBlocking", mock.Anything, 5*time.Minute, []string{"g1", "g2"}).
			Return(nil)

		duration := "5m"
		groups := "g1,g2"
		resp, err := rt.client.DisableBlockingWithResponse(t.Context(), &DisableBlockingParams{
			Duration: &duration,
			Groups:   &groups,
		})
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode())
	})

	t.Run("DisableBlocking rejects a malformed duration with 400", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		bad := "not-a-duration"

		resp, err := rt.client.DisableBlockingWithResponse(t.Context(), &DisableBlockingParams{Duration: &bad})
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode())
	})

	t.Run("DisableBlocking reports control failures with 400", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		rt.control.On("DisableBlocking", mock.Anything, time.Duration(0), []string(nil)).
			Return(errors.New("unknown group"))

		resp, err := rt.client.DisableBlockingWithResponse(t.Context(), &DisableBlockingParams{})
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode())
		require.Contains(t, string(resp.Body), "unknown group")
	})

	t.Run("EnableBlocking reports success", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		rt.control.On("EnableBlocking", mock.Anything)

		resp, err := rt.client.EnableBlockingWithResponse(t.Context())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode())
	})

	t.Run("CacheFlush reports success", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		rt.cacheControl.On("FlushCaches", mock.Anything)

		resp, err := rt.client.CacheFlushWithResponse(t.Context())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode())
	})

	t.Run("ListRefresh reports success", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		rt.refresher.On("RefreshLists", mock.Anything).Return(nil)

		resp, err := rt.client.ListRefreshWithResponse(t.Context())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode())
	})

	t.Run("ListRefresh reports failures with 500", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		rt.refresher.On("RefreshLists", mock.Anything).Return(errors.New("download failed"))

		resp, err := rt.client.ListRefreshWithResponse(t.Context())
		require.NoError(t, err)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode())
		require.Contains(t, string(resp.Body), "download failed")
	})

	t.Run("GetStats returns the snapshot when enabled", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		now := time.Now().Truncate(time.Hour)
		rt.provider.On("StatsEnabled").Return(true)
		rt.provider.On("Stats").Return(stats.Result{
			Start:   now,
			End:     now.Add(24 * time.Hour),
			Summary: stats.Summary{Queries: 42, Cached: 10, CacheHitRate: 0.5},
			PerHour: []stats.HourPoint{{Hour: now, Queries: 42, Blocked: 1, Filtered: 2}},
			TopDomains: []stats.NameCount{
				{Name: "example.com", Count: 7},
			},
		})

		resp, err := rt.client.GetStatsWithResponse(t.Context())
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, resp.StatusCode())
		require.NotNil(t, resp.JSON200)
		require.Equal(t, 42, resp.JSON200.Summary.Queries)
		require.Len(t, resp.JSON200.PerHour, 1)
		require.Equal(t, "example.com", resp.JSON200.TopDomains[0].Name)
	})

	t.Run("GetStats returns 503 when statistics are disabled", func(t *testing.T) {
		rt := newAPIRoundTrip(t)
		rt.provider.On("StatsEnabled").Return(false)

		resp, err := rt.client.GetStatsWithResponse(t.Context())
		require.NoError(t, err)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode())
	})
}
