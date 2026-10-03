package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpt-load/internal/httpclient"
	"gpt-load/internal/models"
	"gpt-load/internal/utils"

	"gorm.io/datatypes"
)

// modelListFixture creates a draft with an isolated HTTP client and no persistent state.
func modelListFixture(t *testing.T, kind, upstream string) (*Factory, *models.Group, *models.APIKey) {
	t.Helper()
	upstreams, err := json.Marshal([]map[string]any{{"url": upstream, "weight": 1}})
	if err != nil {
		t.Fatal(err)
	}
	settings := utils.DefaultSystemSettings()
	settings.ProxyURL = ""
	return NewFactory(nil, httpclient.NewHTTPClientManager()), &models.Group{
		ID: 7, Name: "draft", ChannelType: kind, Upstreams: datatypes.JSON(upstreams),
		EffectiveConfig: settings,
		HeaderRuleList: []models.HeaderRule{
			{Key: "X-Draft", Value: "${GROUP_NAME}:${API_KEY}", Action: "set"},
			{Key: "Accept", Action: "remove"},
		},
	}, &models.APIKey{KeyValue: "test-secret"}
}

// TestFetchModelsProtocols verifies paths, authentication, headers, pagination and raw model IDs.
func TestFetchModelsProtocols(t *testing.T) {
	for _, kind := range []string{"openai", "openai-response", "gemini", "anthropic"} {
		t.Run(kind, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				path := "/prefix/v1/models"
				if kind == "gemini" {
					path = "/prefix/v1beta/models"
				}
				if r.Method != http.MethodGet || r.URL.Path != path {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("X-Draft") != "draft:test-secret" || r.Header.Get("Accept") != "" {
					t.Errorf("draft header rules not applied: %v", r.Header)
				}
				switch kind {
				case "openai", "openai-response":
					if r.Header.Get("Authorization") != "Bearer test-secret" {
						t.Error("missing Bearer key")
					}
					fmt.Fprint(w, `{"data":[{"id":"alpha"},{"id":"beta"},{"id":"alpha"}]}`)
				case "gemini":
					if r.URL.Query().Get("key") != "test-secret" {
						t.Error("missing Gemini key")
					}
					if requests == 1 {
						fmt.Fprint(w, `{"models":[{"name":"models/alpha","supportedGenerationMethods":["embedContent"]}],"nextPageToken":"next + token"}`)
					} else {
						if r.URL.Query().Get("pageToken") != "next + token" {
							t.Error("missing Gemini page token")
						}
						fmt.Fprint(w, `{"models":[{"name":"models/alpha"},{"name":"models/beta"}]}`)
					}
				case "anthropic":
					if r.Header.Get("X-Api-Key") != "test-secret" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
						t.Error("missing Anthropic headers")
					}
					if requests == 1 {
						fmt.Fprint(w, `{"data":[{"id":"alpha"}],"has_more":true,"last_id":"alpha"}`)
					} else {
						if r.URL.Query().Get("after_id") != "alpha" {
							t.Error("missing Anthropic cursor")
						}
						fmt.Fprint(w, `{"data":[{"id":"alpha"},{"id":"beta"}],"has_more":false}`)
					}
				}
			}))
			defer server.Close()
			factory, group, key := modelListFixture(t, kind, server.URL+"/prefix/")
			cached, err := factory.GetChannel(group)
			if err != nil {
				t.Fatal(err)
			}
			group.TestModel = "unsaved-model"
			group.ModelRedirectRules = datatypes.JSONMap{"alpha": "alias"}
			ids, err := FetchModels(context.Background(), factory, group, key)
			if err != nil || !reflect.DeepEqual(ids, []string{"alpha", "beta"}) {
				t.Fatalf("got %v, %v", ids, err)
			}
			wantRequests := 1
			if kind == "gemini" || kind == "anthropic" {
				wantRequests = 2
			}
			if requests != wantRequests || factory.channelCache[group.ID] != cached {
				t.Fatalf("requests=%d, or draft replaced the cached channel", requests)
			}
		})
	}
}

// TestFetchModelsResponses requires complete valid pages and safe errors instead of partial results.
func TestFetchModelsResponses(t *testing.T) {
	for _, tc := range []struct {
		name, kind, first, second, reason string
		status                            int
	}{
		{name: "empty", kind: "openai", first: `{"data":[]}`},
		{name: "invalid JSON", kind: "openai", first: `test-secret`, reason: "invalid_response"},
		{name: "missing list", kind: "openai", first: `{}`, reason: "invalid_response"},
		{name: "missing ID", kind: "openai", first: `{"data":[{}]}`, reason: "invalid_response"},
		{name: "HTTP 401", kind: "gemini", status: 401, first: `test-secret`, reason: "upstream_status"},
		{name: "later page failure", kind: "gemini", first: `{"models":[{"name":"models/alpha"}],"nextPageToken":"next"}`, second: `test-secret`, reason: "invalid_response"},
		{name: "repeated token", kind: "gemini", first: `{"models":[],"nextPageToken":"next"}`, second: `{"models":[],"nextPageToken":"next"}`, reason: "invalid_response"},
		{name: "missing cursor", kind: "anthropic", first: `{"data":[],"has_more":true}`, reason: "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				body := tc.first
				if requests > 1 {
					body = tc.second
				}
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			factory, group, key := modelListFixture(t, tc.kind, server.URL)
			ids, err := FetchModels(context.Background(), factory, group, key)
			if tc.reason == "" {
				if err != nil || ids == nil || len(ids) != 0 {
					t.Fatalf("expected non-nil empty list, got %v, %v", ids, err)
				}
				return
			}
			var listErr *ModelListError
			if ids != nil || !errors.As(err, &listErr) || listErr.Reason != tc.reason || listErr.StatusCode != tc.status {
				t.Fatalf("got partial list or incorrect error: %v, %v", ids, err)
			}
			if strings.Contains(err.Error(), key.KeyValue) {
				t.Fatal("error exposed the key")
			}
		})
	}
}

// TestFetchModelsProxyAndCancellation checks draft proxy routing and context termination.
func TestFetchModelsProxyAndCancellation(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "model-query.invalid" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("unexpected proxy request: %s", r.URL)
		}
		fmt.Fprint(w, `{"data":[{"id":"via-proxy"}]}`)
	}))
	defer proxy.Close()
	factory, group, key := modelListFixture(t, "openai", "http://model-query.invalid")
	group.EffectiveConfig.ProxyURL = proxy.URL
	ids, err := FetchModels(context.Background(), factory, group, key)
	if err != nil || !reflect.DeepEqual(ids, []string{"via-proxy"}) {
		t.Fatalf("draft proxy not used: %v, %v", ids, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ids, err := FetchModels(ctx, factory, group, key); ids != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v, %v", ids, err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	_, err = FetchModels(ctx, factory, group, key)
	var listErr *ModelListError
	if !errors.As(err, &listErr) || listErr.Reason != "timeout" {
		t.Fatalf("deadline not classified: %v", err)
	}
}
