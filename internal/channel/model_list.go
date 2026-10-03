package channel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"gpt-load/internal/models"
	"gpt-load/internal/utils"
)

// ModelListError exposes only safe error categories, never upstream URLs, keys or response bodies.
type ModelListError struct {
	Reason     string
	StatusCode int
}

// Error returns the safe category for logging and error handling.
func (e *ModelListError) Error() string { return "model list: " + e.Reason }

// FetchModels reads all model pages through an uncached channel with the supplied draft settings.
func FetchModels(ctx context.Context, factory *Factory, group *models.Group, key *models.APIKey) ([]string, error) {
	constructor, ok := channelRegistry[group.ChannelType]
	if !ok {
		return nil, &ModelListError{Reason: "request_failed"}
	}
	ch, err := constructor(factory, group)
	if err != nil {
		return nil, &ModelListError{Reason: "request_failed"}
	}
	path := "/v1/models"
	if group.ChannelType == "gemini" {
		path = "/v1beta/models"
	}
	endpoint, err := ch.BuildUpstreamURL(&url.URL{Path: path}, group.Name)
	if err != nil {
		return nil, &ModelListError{Reason: "request_failed"}
	}

	ids := make([]string, 0)
	seenIDs := make(map[string]bool)
	seenCursors := make(map[string]bool)
	cursor := ""
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, &ModelListError{Reason: "request_failed"}
		}
		if cursor != "" {
			query := req.URL.Query()
			if group.ChannelType == "gemini" {
				query.Set("pageToken", cursor)
			} else {
				query.Set("after_id", cursor)
			}
			req.URL.RawQuery = query.Encode()
		}
		req.Header.Set("Accept", "application/json")
		ch.ModifyRequest(req, key, group)
		utils.ApplyHeaderRules(req, group.HeaderRuleList, utils.NewHeaderVariableContext(group, key))
		resp, err := ch.GetHTTPClient().Do(req)
		if err != nil {
			return nil, modelListRequestError(ctx, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, &ModelListError{Reason: "upstream_status", StatusCode: resp.StatusCode}
		}
		// Bound each response while allowing full pagination within the caller's deadline.
		const maxPageBytes = 8 << 20
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
		resp.Body.Close()
		if err != nil {
			return nil, modelListRequestError(ctx, err)
		}
		if len(body) > maxPageBytes {
			return nil, &ModelListError{Reason: "invalid_response"}
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
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, &ModelListError{Reason: "invalid_response"}
		}
		pageIDs := make([]string, 0)
		nextCursor := ""
		if group.ChannelType == "gemini" {
			if page.Models == nil {
				return nil, &ModelListError{Reason: "invalid_response"}
			}
			for _, model := range page.Models {
				pageIDs = append(pageIDs, strings.TrimPrefix(model.Name, "models/"))
			}
			nextCursor = page.NextPageToken
		} else {
			if page.Data == nil {
				return nil, &ModelListError{Reason: "invalid_response"}
			}
			for _, model := range page.Data {
				pageIDs = append(pageIDs, model.ID)
			}
			if group.ChannelType == "anthropic" && page.HasMore {
				nextCursor = page.LastID
				if nextCursor == "" {
					return nil, &ModelListError{Reason: "invalid_response"}
				}
			}
		}
		for _, id := range pageIDs {
			if strings.TrimSpace(id) == "" {
				return nil, &ModelListError{Reason: "invalid_response"}
			}
			if !seenIDs[id] {
				seenIDs[id] = true
				ids = append(ids, id)
			}
		}
		if nextCursor == "" {
			return ids, nil
		}
		if seenCursors[nextCursor] {
			return nil, &ModelListError{Reason: "invalid_response"}
		}
		seenCursors[nextCursor] = true
		cursor = nextCursor
	}
}

// modelListRequestError classifies transport errors without retaining secret-bearing URLs.
func modelListRequestError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	var networkErr net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &networkErr) && networkErr.Timeout()) {
		return &ModelListError{Reason: "timeout"}
	}
	return &ModelListError{Reason: "request_failed"}
}
