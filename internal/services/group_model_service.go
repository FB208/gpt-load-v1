package services

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"gpt-load/internal/channel"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"

	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// GroupModelsParams contains only the draft settings needed to query upstream models.
type GroupModelsParams struct {
	Name        string              `json:"name"`
	ChannelType string              `json:"channel_type"`
	UpstreamURL string              `json:"upstream_url"`
	Config      map[string]any      `json:"config"`
	HeaderRules []models.HeaderRule `json:"header_rules"`
}

// FetchModels reads one active key and queries models without saving the draft or rotating keys.
func (s *GroupService) FetchModels(ctx context.Context, groupID uint, params GroupModelsParams) ([]string, error) {
	var group models.Group
	if err := s.db.WithContext(ctx).First(&group, groupID).Error; err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	if group.GroupType == "aggregate" {
		return nil, NewI18nError(app_errors.ErrValidation, "models.standard_group_required", nil)
	}

	name := strings.TrimSpace(params.Name)
	if !isValidGroupName(name) {
		return nil, NewI18nError(app_errors.ErrValidation, "validation.invalid_group_name", nil)
	}
	channelType := strings.TrimSpace(params.ChannelType)
	if !s.isValidChannelType(channelType) {
		return nil, NewI18nError(app_errors.ErrValidation, "validation.invalid_channel_type", map[string]any{"types": strings.Join(s.channelRegistry, ", ")})
	}
	upstreamURL := strings.TrimSpace(params.UpstreamURL)
	u, err := url.Parse(upstreamURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, NewI18nError(app_errors.ErrValidation, "models.invalid_upstream", nil)
	}
	cleanedConfig, err := s.validateAndCleanConfig(params.Config)
	if err != nil {
		return nil, err
	}
	headerJSON, err := s.normalizeHeaderRules(params.HeaderRules)
	if err != nil {
		return nil, err
	}

	effectiveConfig := s.settingsManager.GetEffectiveConfig(datatypes.JSONMap(cleanedConfig))
	if effectiveConfig.ProxyURL != "" {
		proxyURL, err := url.Parse(effectiveConfig.ProxyURL)
		if err != nil || proxyURL.Hostname() == "" || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https" && proxyURL.Scheme != "socks5" && proxyURL.Scheme != "socks5h") {
			return nil, NewI18nError(app_errors.ErrValidation, "models.invalid_proxy", nil)
		}
	}

	var key models.APIKey
	err = s.db.WithContext(ctx).Where("group_id = ? AND status = ?", groupID, models.KeyStatusActive).Order("id ASC").First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, NewI18nError(app_errors.ErrNoActiveKeys, "models.no_active_key", nil)
	}
	if err != nil {
		return nil, app_errors.ParseDBError(err)
	}
	key.KeyValue, err = s.encryptionSvc.Decrypt(key.KeyValue)
	if err != nil {
		return nil, NewI18nError(app_errors.ErrInternalServer, "models.decrypt_failed", nil)
	}

	// The temporary group has exactly one upstream and never enters the channel cache.
	upstreams, err := json.Marshal([]struct {
		URL    string `json:"url"`
		Weight int    `json:"weight"`
	}{{URL: upstreamURL, Weight: 1}})
	if err != nil {
		return nil, app_errors.ErrInternalServer
	}
	draft := models.Group{
		ID: groupID, Name: name, ChannelType: channelType,
		Upstreams:       datatypes.JSON(upstreams),
		EffectiveConfig: effectiveConfig,
	}
	if len(headerJSON) > 0 {
		if err := json.Unmarshal(headerJSON, &draft.HeaderRuleList); err != nil {
			return nil, app_errors.ErrInternalServer
		}
	}

	// Keep the entire paginated request within the management client's timeout.
	queryCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ids, err := channel.FetchModels(queryCtx, s.channelFactory, &draft, &key)
	if err == nil {
		return ids, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var listErr *channel.ModelListError
	if errors.As(err, &listErr) {
		return nil, NewI18nError(app_errors.ErrBadGateway, "models."+listErr.Reason, map[string]any{"status": listErr.StatusCode})
	}
	return nil, NewI18nError(app_errors.ErrBadGateway, "models.request_failed", nil)
}
