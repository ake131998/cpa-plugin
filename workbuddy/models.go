// models.go implements the ModelProvider capability: static and per-auth
// model lists, dynamic model discovery via the upstream models API, alias
// reverse resolution (client-facing alias → upstream model id), and the
// host-config oauth-excluded-models filter.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func wbModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		{ID: "glm-5.2", Name: "GLM-5.2", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5.1", Name: "GLM-5.1", ContextLength: 131072, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "glm-5v-turbo", Name: "GLM-5V Turbo", ContextLength: 131072, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kimi-k2.7", Name: "Kimi K2.7", ContextLength: 262144, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "minimax-m3", Name: "MiniMax M3", ContextLength: 204800, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "hy3", Name: "Hy3", ContextLength: 262144, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "hy3-preview", Name: "Hy3 Preview", ContextLength: 262144, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "hy3-preview-agent", Name: "Hy3 Preview Agent", ContextLength: 262144, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
}

func cachedDynamicModels() ([]pluginapi.ModelInfo, bool) {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.models) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsCacheTTL {
		return dynamicModelsCache.models, true
	}
	return nil, false
}

func storeDynamicModels(models []pluginapi.ModelInfo) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.models = models
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
}

func fetchDynamicModelsFromStorage(storageJSON []byte) []pluginapi.ModelInfo {
	if models, ok := cachedDynamicModels(); ok {
		refreshEnterpriseIfStale(storageJSON)
		return mergeEnterprise(storageJSON, models)
	}
	accessToken := ""
	if len(storageJSON) > 0 {
		if tok, ok := extractAccessToken(storageJSON); ok {
			accessToken = tok
		}
	}
	if accessToken == "" {
		refreshEnterpriseIfStale(storageJSON)
		return mergeEnterprise(storageJSON, wbModels())
	}
	if dyn, err := callModelsAPI(accessToken); err == nil && len(dyn) > 0 {
		storeDynamicModels(dyn)
		refreshEnterpriseIfStale(storageJSON)
		return mergeEnterprise(storageJSON, dyn)
	}
	refreshEnterpriseIfStale(storageJSON)
	return mergeEnterprise(storageJSON, wbModels())
}

// mergeEnterprise overlays the enterprise custom model list (cached per
// enterpriseId) on top of base. Enterprise models win on ID collisions
// (admin-configured override); new IDs are appended in enterprise order.
// The merged result is computed fresh on every call — the cache only holds
// the pure enterprise list, so a proactive refresh is immediately visible.
func mergeEnterprise(storageJSON []byte, base []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	enterpriseID := ""
	if len(storageJSON) > 0 {
		if sa, err := parseStored(storageJSON); err == nil {
			// JWT claim fallback included: accounts whose auth file lacks
			// enterpriseId can still surface enterprise models from the token.
			enterpriseID = enterpriseIDFor(sa)
		}
	}
	entry, ok := cachedEnterpriseModels(enterpriseID)
	if !ok || len(entry.models) == 0 {
		return base
	}
	out := make([]pluginapi.ModelInfo, len(base))
	copy(out, base)
	byID := make(map[string]int, len(base))
	for i, m := range base {
		byID[strings.ToLower(m.ID)] = i
	}
	for _, em := range entry.models {
		if i, hit := byID[strings.ToLower(em.ID)]; hit {
			out[i] = em // enterprise overrides in place
			continue
		}
		out = append(out, em)
	}
	return out
}

// fetchDynamicModels calls the WorkBuddy API to get the latest model list.
// Falls back to the hardcoded list on any error.
// extractAccessToken handles both flat (CPA UI) and nested (plugin OAuth) auth file shapes.
func extractAccessToken(raw []byte) (string, bool) {
	// flat shape from CPA-Manager-Plus UI
	var flat struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &flat); err == nil && strings.TrimSpace(flat.AccessToken) != "" {
		return flat.AccessToken, true
	}
	// nested shape from plugin OAuth
	var nested storedAuth
	if err := json.Unmarshal(raw, &nested); err == nil && strings.TrimSpace(nested.Auth.AccessToken) != "" {
		return nested.Auth.AccessToken, true
	}
	return "", false
}

// realmFromToken decodes the JWT iss claim to determine the account realm.
// Global tokens have iss=...workbuddy.ai...; CN tokens have iss=...codebuddy.cn...
// Returns true if the token is Global.
func isGlobalToken(accessToken string) bool {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return false
	}
	payload := parts[1]
	// base64url padding
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return false
	}
	var claims struct {
		ISS string `json:"iss"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return false
	}
	return strings.Contains(strings.ToLower(claims.ISS), "workbuddy.ai")
}

// callModelsAPI GETs /console/enterprises/personal/models from the upstream.
// Uses the shared client (connection pooling) with a per-request 15s budget;
// the shared client's own 120s timeout stays as the outer bound.
func callModelsAPI(accessToken string) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Model discovery is per-realm: Global tokens must query workbuddy.ai,
	// not copilot.tencent.com (which 500s for Global tokens). Decode JWT iss.
	isGlobal := isGlobalToken(accessToken)
	modelsURL := endpointModels
	origin := originReferer
	if isGlobal {
		modelsURL = upstreamBaseGlobal + "/console/enterprises/personal/models"
		origin = originRefererGlobal
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	body := resp.Body
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models API status %d", resp.StatusCode)
	}
	var apiResp struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID                 string          `json:"id"`
				Name               string          `json:"name"`
				Description        string          `json:"description"`
				Credits            string          `json:"credits"`
				Configurable       bool            `json:"configurable"`
				Configured         bool            `json:"configured"`
				IsDefault          bool            `json:"isDefault"`
				SupportsImages     bool            `json:"supportsImages"`
				SupportsReasoning  bool            `json:"supportsReasoning"`
				OnlyReasoning      bool            `json:"onlyReasoning"`
				Reasoning          json.RawMessage `json:"reasoning"`
				DisabledMultimodal bool            `json:"disabledMultimodal"`
				Disabled           bool            `json:"disabled"`
				DisabledReason     string          `json:"disabledReason"`
				ContextWindow      json.RawMessage `json:"contextWindow"`
				MaxTokens          json.RawMessage `json:"maxTokens"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, err
	}
	if apiResp.Code != 0 {
		return nil, fmt.Errorf("models API code %d", apiResp.Code)
	}
	var cliModelIDs []string
	for _, a := range apiResp.Data.Agents {
		if a.Name == "cli" {
			cliModelIDs = a.Models
			break
		}
	}
	if len(cliModelIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]struct {
		ID                 string          `json:"id"`
		Name               string          `json:"name"`
		Description        string          `json:"description"`
		Credits            string          `json:"credits"`
		Configurable       bool            `json:"configurable"`
		Configured         bool            `json:"configured"`
		IsDefault          bool            `json:"isDefault"`
		SupportsImages     bool            `json:"supportsImages"`
		SupportsReasoning  bool            `json:"supportsReasoning"`
		OnlyReasoning      bool            `json:"onlyReasoning"`
		Reasoning          json.RawMessage `json:"reasoning"`
		DisabledMultimodal bool            `json:"disabledMultimodal"`
		Disabled           bool            `json:"disabled"`
		DisabledReason     string          `json:"disabledReason"`
		ContextWindow      json.RawMessage `json:"contextWindow"`
		MaxTokens          json.RawMessage `json:"maxTokens"`
	}, len(apiResp.Data.Models))
	for _, m := range apiResp.Data.Models {
		dynMap[m.ID] = m
	}
	var out []pluginapi.ModelInfo
	for _, id := range cliModelIDs {
		m, ok := dynMap[id]
		if !ok {
			continue
		}
		if m.Disabled {
			continue
		}
		ctxLen := int64(0)
		if len(m.ContextWindow) > 0 {
			var v float64
			if err := json.Unmarshal(m.ContextWindow, &v); err == nil {
				ctxLen = int64(v)
			}
		}
		maxTok := int64(0)
		if len(m.MaxTokens) > 0 {
			var v float64
			if err := json.Unmarshal(m.MaxTokens, &v); err == nil {
				maxTok = int64(v)
			}
		}
		out = append(out, pluginapi.ModelInfo{
			ID:                         m.ID,
			Name:                       m.Name,
			ContextLength:              ctxLen,
			MaxCompletionTokens:        maxTok,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	return out, nil
}

// enterpriseModelsBaseCN / enterpriseModelsBaseGlobal are the config/models
// endpoint templates per realm ("%s" = enterpriseId). Vars (not consts) so
// tests can point them at an httptest server — billingBase pattern.
var (
	enterpriseModelsBaseCN     = endpointEnterpriseModels
	enterpriseModelsBaseGlobal = upstreamBaseGlobal + "/console/enterprises/%s/config/models"
)

// callEnterpriseModelsAPI GETs /console/enterprises/<enterpriseId>/config/models
// — the enterprise-admin-defined custom model list. Uses the same realm
// routing as callModelsAPI (Global tokens must hit workbuddy.ai). The
// X-User-Id / X-Enterprise-Id / X-Tenant-Id / X-Domain headers mirror the
// reference client's auth interceptor (ModelsProductProvider), which the
// backend uses to resolve the requesting enterprise. Any error returns nil so
// callers silently fall back to the default list.
func callEnterpriseModelsAPI(accessToken, enterpriseID, userID, domain string) ([]pluginapi.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	modelsURL := fmt.Sprintf(enterpriseModelsBaseCN, enterpriseID)
	origin := originReferer
	if isGlobalToken(accessToken) {
		modelsURL = fmt.Sprintf(enterpriseModelsBaseGlobal, enterpriseID)
		origin = originRefererGlobal
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(userID) != "" {
		req.Header.Set("X-User-Id", userID)
	}
	req.Header.Set("X-Enterprise-Id", enterpriseID)
	req.Header.Set("X-Tenant-Id", enterpriseID)
	if strings.TrimSpace(domain) != "" {
		req.Header.Set("X-Domain", domain)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	// Debug mirror: the config/models response shape is non-contractual and
	// unverifiable via curl (needs an OAuth bearer), so the raw body is
	// dumped when WB_UPSTREAM_DUMP_DIR is set.
	dumpUpstreamResponse("enterprise_models_"+enterpriseID, http.MethodGet, modelsURL, resp.StatusCode, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("enterprise models API status %d", resp.StatusCode)
	}
	return parseEnterpriseModels(resp.Body)
}

// parseEnterpriseModels decodes the config/models response tolerantly — the
// exact shape is not contractual. Accepted forms (checked in order):
//
//	[...]                              direct array
//	{models:[...]}                     named array
//	{data:[...]}                       wrapped array
//	{data:{models:[...]}}              nested object
//	{data:{data:[...]}}                double-nested (observed upstream shape)
//	{code,msg,data:{models:[...]}}     apiEnvelope shape
//
// Each model tolerates id/name/context/max-token fields in camelCase or
// snake_case, as JSON numbers or numeric strings. Disabled models are
// dropped. Models whose tags field is present but lacks "chat" are dropped
// too — the reference client only feeds models tagged "chat" into the chat
// agent's list; untagged models are kept for shape compatibility.
func parseEnterpriseModels(raw []byte) ([]pluginapi.ModelInfo, error) {
	var list []json.RawMessage
	if !extractModelArray(raw, &list) {
		return nil, fmt.Errorf("enterprise models: unrecognized response shape")
	}
	if len(list) == 0 {
		// A well-formed but empty list is a legitimate state — verified
		// against the real upstream, which answers {"code":0,"data":[]} when
		// the enterprise has no custom models configured. Not an error: the
		// cached entry stays fresh with model_count 0.
		return nil, nil
	}
	out := make([]pluginapi.ModelInfo, 0, len(list))
	for _, item := range list {
		m, tags, err := parseEnterpriseModel(item)
		if err != nil {
			continue
		}
		if len(tags) > 0 && !containsFold(tags, "chat") {
			continue // tagged but not a chat model (agent/skill entry)
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("enterprise models: no parseable models")
	}
	return out, nil
}

// containsFold reports whether tags contains value (case-insensitive).
func containsFold(tags []string, value string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), value) {
			return true
		}
	}
	return false
}

// extractModelArray locates the model array inside any of the tolerated
// response shapes. Returns false when none matches.
func extractModelArray(raw []byte, list *[]json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if trimmed[0] == '[' {
		return json.Unmarshal(trimmed, list) == nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return false
	}
	for _, key := range []string{"models", "data"} {
		v, ok := probe[key]
		if !ok || len(v) == 0 {
			continue
		}
		t := bytes.TrimSpace(v)
		if t[0] == '[' {
			return json.Unmarshal(t, list) == nil
		}
		// {data:{models:[...]}} / {data:{data:[...]}} / {code,data:{models:[...]}}
		var nested map[string]json.RawMessage
		if json.Unmarshal(t, &nested) == nil {
			for _, nk := range []string{"models", "data"} {
				if nv, ok2 := nested[nk]; ok2 && len(nv) > 0 && bytes.TrimSpace(nv)[0] == '[' {
					return json.Unmarshal(nv, list) == nil
				}
			}
		}
		if key == "models" {
			return false
		}
	}
	return false
}

// parseEnterpriseModel converts one model object into ModelInfo, tolerating
// camelCase/snake_case and numeric-string fields. A missing/invalid id drops
// the model; disabled models are skipped. The tags slice is returned so the
// caller can apply the reference client's "chat"-tag filter.
func parseEnterpriseModel(item json.RawMessage) (pluginapi.ModelInfo, []string, error) {
	var m struct {
		ID                 any      `json:"id"`
		Name               any      `json:"name"`
		Tags               []string `json:"tags"`
		ContextWindow      any      `json:"contextWindow"`
		Context            any      `json:"context"`
		ContextWindowSnake any      `json:"context_window"`
		MaxTokens          any      `json:"maxTokens"`
		MaxToken           any      `json:"maxToken"`
		MaxTokensSnake     any      `json:"max_tokens"`
		Disabled           any      `json:"disabled"`
	}
	if err := json.Unmarshal(item, &m); err != nil {
		return pluginapi.ModelInfo{}, nil, err
	}
	id, _ := toString(m.ID)
	if strings.TrimSpace(id) == "" {
		return pluginapi.ModelInfo{}, nil, fmt.Errorf("model missing id")
	}
	if disabled, _ := toBool(m.Disabled); disabled {
		return pluginapi.ModelInfo{}, nil, fmt.Errorf("model disabled")
	}
	name := id
	if n, ok := toString(m.Name); ok && strings.TrimSpace(n) != "" {
		name = n
	}
	ctxLen := firstNumeric(m.ContextWindow, m.Context, m.ContextWindowSnake)
	maxTok := firstNumeric(m.MaxTokens, m.MaxToken, m.MaxTokensSnake)
	return pluginapi.ModelInfo{
		ID:                         id,
		Name:                       name,
		ContextLength:              ctxLen,
		MaxCompletionTokens:        maxTok,
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}, m.Tags, nil
}

func toString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case float64:
		return fmt.Sprintf("%g", t), true
	}
	return "", false
}

func toBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		return b, err == nil
	case float64:
		return t != 0, true
	}
	return false, false
}

func firstNumeric(vals ...any) int64 {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case float64:
			return int64(t)
		case json.Number:
			if i, err := t.Int64(); err == nil {
				return i
			}
			if f, err := t.Float64(); err == nil {
				return int64(f)
			}
		case string:
			if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
				return i
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return int64(f)
			}
		}
	}
	return 0
}

func cacheModelAliases(host pluginapi.HostConfigSummary) {
	entries := host.OAuthModelAlias[providerName]
	if len(entries) == 0 {
		// Host may key the channel case-insensitively; fall back to a scan.
		for channel, list := range host.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				entries = list
				break
			}
		}
	}
	byAlias := make(map[string]string, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		alias := strings.TrimSpace(e.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		byAlias[strings.ToLower(alias)] = name
	}
	modelAliasCache.Lock()
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
}

// resolveUpstreamModel maps an aliased requested model back to the real
// upstream model ID. Returns the input unchanged when nothing matches.
func resolveUpstreamModel(model string, attributes map[string]string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return model
	}
	key := strings.ToLower(m)
	if name, ok := parseModelAliasAttribute(attributes)[key]; ok {
		return name
	}
	modelAliasCache.RLock()
	name, ok := modelAliasCache.byAlias[key]
	modelAliasCache.RUnlock()
	if ok {
		return name
	}
	return m
}

// parseModelAliasAttribute decodes a per-auth alias override from auth
// attributes. Accepts JSON ([{"name":...,"alias":...}] or {alias:name}) or
// comma-separated "alias=name" pairs.
func parseModelAliasAttribute(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	raw := ""
	for _, k := range []string{"model_alias", "model-alias", "oauth-model-alias"} {
		if v := strings.TrimSpace(attributes[k]); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	add := func(name, alias string) {
		name, alias = strings.TrimSpace(name), strings.TrimSpace(alias)
		if name != "" && alias != "" && !strings.EqualFold(name, alias) {
			out[strings.ToLower(alias)] = name
		}
	}
	if strings.HasPrefix(raw, "[") {
		var list []struct {
			Name  string `json:"name"`
			Alias string `json:"alias"`
		}
		if json.Unmarshal([]byte(raw), &list) == nil {
			for _, e := range list {
				add(e.Name, e.Alias)
			}
			return out
		}
	}
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(raw), &m) == nil {
			for alias, name := range m {
				add(name, alias)
			}
			return out
		}
	}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			add(kv[1], kv[0])
		}
	}
	return out
}

// filterExcludedModels removes models listed in oauth-excluded-models for
// the workbuddy provider. The host passes this config via HostConfigSummary.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	// Try exact provider match, then case-insensitive scan.
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	// Use a fresh slice — models[:0] would alias the input's backing array,
	// which may be the dynamicModelsCache's own slice. Mutating it in place
	// would corrupt the cache for subsequent callers (P0 bug: after one
	// filterExcludedModels call, cache returns the filtered list as the
	// "full" list on the next fetch).
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}

// publishUsage reports one upstream attempt into CPAMP request monitoring.
// requestedModel is client-facing (may be alias); upstreamModel is resolved.

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	models := wbModels()
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Always return the plugin's canonical provider key. The host skips any
	// response whose Provider doesn't match the auth's provider, so echoing
	// req.AuthProvider back would silently drop the model list whenever the
	// auth file carries a non-canonical provider string.
	cacheModelAliases(req.Host)
	models := fetchDynamicModelsFromStorage(req.StorageJSON)
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}
