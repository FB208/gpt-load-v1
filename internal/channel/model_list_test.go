package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"gpt-load/internal/httpclient"
	"gpt-load/internal/models"
	"gpt-load/internal/utils"
)

// modelListFixture creates a draft group with a real HTTP client and a known key.
func modelListFixture(t *testing.T, channelType, upstream string) (*Factory, *models.Group, *models.APIKey) {
	t.Helper()
	data, err := json.Marshal([]map[string]any{{"url": upstream + "/prefix", "weight": 1}})
	if err != nil {
		t.Fatal(err)
	}
	return NewFactory(nil, httpclient.NewHTTPClientManager()), &models.Group{
		ID: 7, Name: "draft", ChannelType: channelType, Upstreams: data,
		EffectiveConfig: utils.DefaultSystemSettings(),
		HeaderRuleList:  []models.HeaderRule{{Key: "X-Group", Value: "${GROUP_NAME}", Action: "set"}},
	}, &models.APIKey{KeyValue: "fixture-secret"}
}

// TestFetchModels verifies native endpoints, authentication, full pagination and deduplication.
func TestFetchModels(t *testing.T) {
	for _, tc := range []struct {
		channel string
		pages   []string
		want    []string
	}{
		{"openai", []string{`{"data":[{"id":"one"},{"id":"one"},{"id":"two"}]}`}, []string{"one", "two"}},
		{"openai-response", []string{`{"data":[{"id":"one"},{"id":"two"}]}`}, []string{"one", "two"}},
		{"gemini", []string{`{"models":[{"name":"models/one"}],"nextPageToken":"next"}`, `{"models":[{"name":"models/one"},{"name":"models/two"}]}`}, []string{"one", "two"}},
		{"anthropic", []string{`{"data":[{"id":"one"}],"has_more":true,"last_id":"next"}`, `{"data":[{"id":"one"},{"id":"two"}],"has_more":false}`}, []string{"one", "two"}},
	} {
		t.Run(tc.channel, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := "/prefix/v1/models"
				switch tc.channel {
				case "gemini":
					path = "/prefix/v1beta/models"
					if r.URL.Query().Get("key") != "fixture-secret" {
						t.Error("missing Gemini key")
					}
					if calls > 0 && r.URL.Query().Get("pageToken") != "next" {
						t.Error("missing pageToken")
					}
				case "anthropic":
					if r.Header.Get("x-api-key") != "fixture-secret" || r.Header.Get("anthropic-version") != "2023-06-01" {
						t.Error("missing Anthropic headers")
					}
					if calls > 0 && r.URL.Query().Get("after_id") != "next" {
						t.Error("missing after_id")
					}
				default:
					if r.Header.Get("Authorization") != "Bearer fixture-secret" {
						t.Error("missing Bearer key")
					}
				}
				if r.Method != http.MethodGet || r.URL.Path != path || r.Header.Get("X-Group") != "draft" {
					t.Errorf("unexpected request: %s %s, headers=%v", r.Method, r.URL.Path, r.Header)
				}
				if calls >= len(tc.pages) {
					t.Error("unexpected extra page")
					w.WriteHeader(500)
					return
				}
				fmt.Fprint(w, tc.pages[calls])
				calls++
			}))
			defer srv.Close()
			factory, group, key := modelListFixture(t, tc.channel, srv.URL)
			// A draft query must leave the existing proxy channel in use.
			saved := *group
			saved.Upstreams = []byte(`[{"url":"https://saved.example","weight":1}]`)
			live, err := factory.GetChannel(&saved)
			if err != nil {
				t.Fatal(err)
			}
			got, err := factory.FetchModels(context.Background(), group, key)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("models = %v, want %v", got, tc.want)
			}
			if calls != len(tc.pages) {
				t.Errorf("requests = %d, want %d", calls, len(tc.pages))
			}
			liveAfter, err := factory.GetChannel(&saved)
			if err != nil || liveAfter != live {
				t.Fatal("draft query replaced the live channel")
			}
			target, err := liveAfter.BuildUpstreamURL(&url.URL{Path: "/v1/models"}, saved.Name)
			if err != nil || target != "https://saved.example/v1/models" {
				t.Fatalf("live upstream changed: %s, %v", target, err)
			}
		})
	}
}

// TestFetchModelsFailures verifies empty lists and safe, all-or-nothing error handling.
func TestFetchModelsFailures(t *testing.T) {
	for _, tc := range []struct {
		name, channel, body, message string
		status                       int
	}{
		{"empty", "openai", `{"data":[]}`, "", 200},
		{"invalid JSON", "openai", "not JSON fixture-secret", "models.invalid_response", 200},
		{"missing list", "openai", `{}`, "models.invalid_response", 200},
		{"upstream unauthorized", "gemini", "fixture-secret", "models.upstream_status", 401},
		{"later page failed", "gemini", `{"models":[{"name":"models/one"}],"nextPageToken":"next"}`, "models.upstream_status", 502},
		{"repeated cursor", "gemini", `{"models":[{"name":"models/one"}],"nextPageToken":"next"}`, "models.invalid_response", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				status := tc.status
				if tc.name == "later page failed" && calls == 1 {
					status = 200
				}
				w.WriteHeader(status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			factory, group, key := modelListFixture(t, tc.channel, srv.URL)
			got, err := factory.FetchModels(context.Background(), group, key)
			if tc.message == "" {
				if err != nil || got == nil || len(got) != 0 {
					t.Fatalf("empty list = %v, %v", got, err)
				}
				return
			}
			failure, ok := err.(*ModelListError)
			if !ok || failure.MessageID != tc.message || got != nil {
				t.Fatalf("models=%v, error=%v", got, err)
			}
			if tc.message == "models.upstream_status" && failure.Status != tc.status {
				t.Errorf("status = %d, want %d", failure.Status, tc.status)
			}
			if strings.Contains(err.Error(), key.KeyValue) {
				t.Error("error exposed credentials")
			}
		})
	}
	t.Run("cancelled request", func(t *testing.T) {
		factory, group, key := modelListFixture(t, "gemini", "http://127.0.0.1:1")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := factory.FetchModels(ctx, group, key)
		if got != nil || err == nil || strings.Contains(err.Error(), key.KeyValue) {
			t.Fatalf("models=%v, error=%v", got, err)
		}
	})
}
