package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"
	"gpt-load/internal/utils"
)

// GeneratedImage contains the image data returned for the test preview.
type GeneratedImage struct {
	B64JSON string `json:"b64_json,omitempty"`
	URL     string `json:"url,omitempty"`
}

// GenerateTestImage sends one non-streaming image request with the selected key.
func (f *Factory) GenerateTestImage(ctx context.Context, group *models.Group, key *models.APIKey, model string) (GeneratedImage, error) {
	ch, err := newOpenAIChannel(f, group)
	if err != nil {
		return GeneratedImage{}, err
	}
	target, err := ch.BuildUpstreamURL(&url.URL{Path: "/v1/images/generations"}, group.Name)
	if err != nil {
		return GeneratedImage{}, err
	}

	body, _ := json.Marshal(map[string]any{
		"model":  model,
		"prompt": "A red apple on a white background",
		"n":      1,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return GeneratedImage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	ch.ModifyRequest(req, key, group)
	utils.ApplyHeaderRules(req, group.HeaderRuleList, utils.NewHeaderVariableContext(group, key))

	client := *ch.GetHTTPClient()
	client.Timeout = 180 * time.Second
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		// Client.Do wraps transport errors in a url.Error; omit the upstream URL.
		return GeneratedImage{}, err.(*url.Error).Err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		if err != nil {
			return GeneratedImage{}, fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
		}
		message := strings.TrimSpace(app_errors.ParseUpstreamError(body))
		if message != "" {
			message = strings.ReplaceAll(message, key.KeyValue, "[redacted]")
			return GeneratedImage{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, message)
		}
		return GeneratedImage{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var result struct {
		Data []GeneratedImage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return GeneratedImage{}, fmt.Errorf("invalid image response: %w", err)
	}
	if len(result.Data) == 0 || (result.Data[0].B64JSON == "" && result.Data[0].URL == "") {
		return GeneratedImage{}, fmt.Errorf("upstream returned no image")
	}
	return result.Data[0], nil
}
