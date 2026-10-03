package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"gpt-load/internal/models"
	"gpt-load/internal/utils"
)

// ModelListError carries a safe, translatable failure without upstream credentials.
type ModelListError struct {
	MessageID string
	Status    int
}

// Error returns the translation key for the model-list failure.
func (e *ModelListError) Error() string { return e.MessageID }

// FetchModels queries the platform using a temporary channel and returns its model IDs.
func (f *Factory) FetchModels(ctx context.Context, group *models.Group, key *models.APIKey) ([]string, error) {
	constructor, ok := channelRegistry[group.ChannelType]
	if !ok {
		return nil, &ModelListError{MessageID: "models.invalid_channel"}
	}
	// Draft settings must not replace the live proxy's cached channel.
	ch, err := constructor(f, group)
	if err != nil {
		return nil, &ModelListError{MessageID: "models.invalid_upstream"}
	}
	path := "/v1/models"
	if group.ChannelType == "gemini" {
		path = "/v1beta/models"
	}
	target, err := ch.BuildUpstreamURL(&url.URL{Path: path}, group.Name)
	if err != nil {
		return nil, &ModelListError{MessageID: "models.invalid_upstream"}
	}

	ids := []string{}
	seenIDs := make(map[string]bool)
	seenPages := make(map[string]bool)
	cursor := ""
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, &ModelListError{MessageID: "models.invalid_upstream"}
		}
		query := req.URL.Query()
		if cursor != "" {
			if group.ChannelType == "gemini" {
				query.Set("pageToken", cursor)
			} else {
				query.Set("after_id", cursor)
			}
		}
		req.URL.RawQuery = query.Encode()
		ch.ModifyRequest(req, key, group)
		utils.ApplyHeaderRules(req, group.HeaderRuleList, utils.NewHeaderVariableContext(group, key))
		resp, err := ch.GetHTTPClient().Do(req)
		if err != nil {
			// Gemini places the key in the URL; never return the raw transport error.
			return nil, &ModelListError{MessageID: "models.request_failed"}
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, &ModelListError{MessageID: "models.upstream_status", Status: resp.StatusCode}
		}
		var page struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
			NextPageToken string `json:"nextPageToken"`
			HasMore       bool   `json:"has_more"`
			LastID        string `json:"last_id"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil || (group.ChannelType != "gemini" && page.Data == nil) || (group.ChannelType == "gemini" && page.Models == nil) {
			return nil, &ModelListError{MessageID: "models.invalid_response"}
		}
		names := make([]string, 0, len(page.Data)+len(page.Models))
		if group.ChannelType == "gemini" {
			for _, model := range page.Models {
				names = append(names, strings.TrimPrefix(model.Name, "models/"))
			}
		} else {
			for _, model := range page.Data {
				names = append(names, model.ID)
			}
		}
		for _, name := range names {
			if name != "" && !seenIDs[name] {
				seenIDs[name] = true
				ids = append(ids, name)
			}
		}
		cursor = ""
		switch group.ChannelType {
		case "gemini":
			cursor = page.NextPageToken
		case "anthropic":
			if page.HasMore {
				cursor = page.LastID
				if cursor == "" {
					return nil, &ModelListError{MessageID: "models.invalid_response"}
				}
			}
		}
		if cursor == "" {
			return ids, nil
		}
		if seenPages[cursor] {
			return nil, &ModelListError{MessageID: "models.invalid_response"}
		}
		seenPages[cursor] = true
	}
}
