// usage.go implements the UsagePlugin capability: the host calls handleUsage
// after every request with a canonical record, and we forward to CPAMP. The
// legacy publishUsage path is kept for hosts without UsagePlugin wiring.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleUsage is the UsagePlugin entry point. The host calls this after every
// request with the canonical usage record (it also records to its own
// DefaultManager, so we don't need to). Our job is just to forward to CPAMP.
//
// v0.7.0 compliance: this replaces the old pattern where each executor path
// called publishUsage directly (which both skipped the host audit and forced
// every call site to remember to publish).
func handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	// Only forward workbuddy's own records; the host will route other plugins'
	// usage to their own UsagePlugin.
	if record.Provider != "" && record.Provider != providerName {
		return okEnvelope(map[string]any{"forwarded": false})
	}
	detail := usage.Detail{
		InputTokens:         record.Detail.InputTokens,
		OutputTokens:        record.Detail.OutputTokens,
		ReasoningTokens:     record.Detail.ReasoningTokens,
		CachedTokens:        record.Detail.CachedTokens,
		CacheReadTokens:     record.Detail.CacheReadTokens,
		CacheCreationTokens: record.Detail.CacheCreationTokens,
		TotalTokens:         record.Detail.TotalTokens,
	}
	started := record.RequestedAt
	if started.IsZero() {
		started = time.Now().Add(-record.Latency)
	}
	// Prefer the canonical AuthIndex when the host fills it; fall back to
	// AuthID (which on some CPA builds is an opaque UUID that does not match
	// host.auth.list entries, breaking account snapshot resolution).
	authID := strings.TrimSpace(record.AuthIndex)
	if authID == "" {
		authID = record.AuthID
	}
	forwardUsageToCPAMP(
		record.Alias,
		record.Model,
		authID,
		record.APIKey,
		started,
		detail,
		record.Failed,
		record.Failure.StatusCode,
		record.Failure.Body,
	)
	return okEnvelope(map[string]any{"forwarded": true})
}

// publishUsage is kept for backward compatibility with existing call sites
// inside the executor. After v0.7.0 the host calls UsagePlugin.HandleUsage
// itself after every request, so executor-side publish is redundant. We
// forward to CPAMP here too so that hosts WITHOUT UsagePlugin wiring (older
// CPA builds) still emit usage; hosts with the wiring will trigger HandleUsage
// separately, but forwardUsageToCPAMP is idempotent at the CPAMP ingestion
// layer (NDJSON import dedups on timestamp+auth+model+total_tokens).
func publishUsage(requestedModel, upstreamModel, authID string, started time.Time, detail usage.Detail, failed bool, statusCode int, errBody string) {
	model := strings.TrimSpace(upstreamModel)
	if model == "" {
		model = strings.TrimSpace(requestedModel)
	}
	alias := strings.TrimSpace(requestedModel)
	if alias == "" {
		alias = model
	}
	// Fire-and-forget so the executor hot path never blocks on the CPAMP
	// round-trip. handleUsage (the host-driven path) is synchronous because the
	// host already runs it on its own goroutine after the request completes.
	go forwardUsageToCPAMP(alias, model, authID, "", started, normalizeUsageDetail(detail), failed, statusCode, errBody)
}

// forwardUsageToCPAMP POSTs one NDJSON line to CPAMP usage/import.
// Silent on misconfig / network errors — never blocks chat.
//
// v0.7.0: routed via host.http.do so the call is captured by request-log and
// uses host transport policy (proxy, timeout). Was: raw http.Client.
func forwardUsageToCPAMP(alias, model, authID, apiKey string, started time.Time, detail usage.Detail, failed bool, statusCode int, errBody string) {
	usageReportMu.RLock()
	url := strings.TrimSpace(usageReportURL)
	key := strings.TrimSpace(usageReportKey)
	usageReportMu.RUnlock()
	if url == "" || key == "" {
		return
	}
	ts := started
	if ts.IsZero() {
		ts = time.Now()
	}
	latencyMs := int64(0)
	if !started.IsZero() {
		latencyMs = time.Since(started).Milliseconds()
		if latencyMs < 0 {
			latencyMs = 0
		}
	}
	total := detail.TotalTokens
	if total == 0 {
		total = detail.InputTokens + detail.OutputTokens + detail.ReasoningTokens
	}
	failBody := ""
	failCode := 200
	if failed {
		failCode = statusCode
		if failCode <= 0 {
			failCode = 502
		}
		failBody = truncate(redactSecrets(errBody), 512)
	}
	nickname := accountNicknameFor(authID)
	payload := map[string]any{
		// event_hash routes the record through CPAMP's exported-record path so
		// the extra dimension fields below are mapped verbatim, and makes the
		// import idempotent (event_hash is unique in CPAMP usage_events).
		"event_hash":             usageEventHash(ts, authID, model, total),
		"timestamp":              ts.UTC().Format(time.RFC3339Nano),
		"latency_ms":             latencyMs,
		"source":                 "workbuddy",
		"auth_index":             strings.TrimSpace(authID),
		"provider":               providerName,
		"model":                  model,
		"alias":                  alias,
		"endpoint":               "POST /v1/chat/completions",
		"auth_type":              "oauth",
		"executor_type":          "workbuddy",
		"generate":               true,
		"failed":                 failed,
		"api_key_hash":           hashToken(apiKey),
		"account_snapshot":       nickname,
		"auth_label_snapshot":    nickname,
		"auth_provider_snapshot": providerName,
		"tokens": map[string]any{
			"input_tokens":          detail.InputTokens,
			"output_tokens":         detail.OutputTokens,
			"reasoning_tokens":      detail.ReasoningTokens,
			"cached_tokens":         detail.CachedTokens,
			"cache_read_tokens":     detail.CacheReadTokens,
			"cache_creation_tokens": detail.CacheCreationTokens,
			"total_tokens":          total,
		},
		"fail": map[string]any{
			"status_code": failCode,
			"body":        failBody,
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	body = append(body, '\n')
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := hostHTTPDo(req)
	if err != nil {
		return
	}
	_ = resp.Body
}

// usageEventHash derives a stable unique event hash for CPAMP's exported-record
// path. Truncated to second precision so the host-driven handleUsage and the
// legacy executor publishUsage for the same request produce the same hash and
// CPAMP's unique event_hash column dedups them; collisions across distinct
// requests (same second + auth + model + identical token total) are negligible.
func usageEventHash(ts time.Time, authID, model string, total int64) string {
	second := ts.UTC().Truncate(time.Second).Format(time.RFC3339)
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", second, authID, model, total)))
	return hex.EncodeToString(h[:])
}

// hashToken hashes a client API key identifier for CPAMP's api_key_hash column.
// Empty input yields an empty value (the field is simply omitted from the view).
func hashToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// accountNicknameCache caches authID → nickname so the per-request usage
// hot path does not issue a host.auth.get RPC on every request.
var accountNicknameCache sync.Map // authID → nicknameCacheEntry

type nicknameCacheEntry struct {
	nickname string
	at       time.Time
}

const nicknameCacheTTL = 10 * time.Minute

// accountNicknameFor resolves a human-readable label for an auth identifier
// via host.auth.list (one RPC for all entries). The host calls usage.handle
// with AuthID which may be the credential ID, the runtime AuthIndex, or (on
// some CPA builds) the account UID — match ID/AuthIndex first, then fall back
// to reading auth file contents and comparing Account.UID.
func accountNicknameFor(authID string) string {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	if v, ok := accountNicknameCache.Load(authID); ok {
		if e, ok2 := v.(nicknameCacheEntry); ok2 && time.Since(e.at) < nicknameCacheTTL {
			return e.nickname
		}
	}
	nickname := resolveAccountNickname(authID)
	accountNicknameCache.Store(authID, nicknameCacheEntry{nickname: nickname, at: time.Now()})
	return nickname
}

func resolveAccountNickname(authID string) string {
	files, err := hostAuthList()
	if err != nil || len(files) == 0 {
		return ""
	}
	for _, f := range files {
		if f.ID == authID || f.AuthIndex == authID {
			if n := strings.TrimSpace(f.Label); n != "" {
				return n
			}
			return strings.TrimSpace(f.Name)
		}
	}
	// Fallback: CPA fills UsageRecord.AuthID with the account UID, which
	// host.auth.list does not expose — resolve it from auth file contents.
	for _, f := range files {
		key := f.AuthIndex
		if key == "" {
			key = f.ID
		}
		if key == "" {
			continue
		}
		sa, err := hostAuthGet(key)
		if err != nil || sa == nil {
			continue
		}
		if sa.Account.UID == authID {
			if n := strings.TrimSpace(sa.Account.Nickname); n != "" {
				return n
			}
			if n := strings.TrimSpace(f.Label); n != "" {
				return n
			}
			return strings.TrimSpace(f.Name)
		}
	}
	return ""
}

func normalizeUsageDetail(d usage.Detail) usage.Detail {
	if d.TotalTokens == 0 {
		if total := d.InputTokens + d.OutputTokens + d.ReasoningTokens; total > 0 {
			d.TotalTokens = total
		}
	}
	return d
}

// usageDetailFromMap converts an OpenAI-style "usage" JSON object into a
// usage.Detail, tolerating both snake_case naming and numeric jitter.
func usageDetailFromMap(m map[string]any) usage.Detail {
	if len(m) == 0 {
		return usage.Detail{}
	}
	num := func(keys ...string) int64 {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				switch n := v.(type) {
				case float64:
					return int64(n)
				case int64:
					return n
				case json.Number:
					i, _ := n.Int64()
					return i
				}
			}
		}
		return 0
	}
	d := usage.Detail{
		InputTokens:     num("prompt_tokens", "input_tokens"),
		OutputTokens:    num("completion_tokens", "output_tokens"),
		TotalTokens:     num("total_tokens"),
		CachedTokens:    num("cached_tokens"),
		CacheReadTokens: num("cache_read_input_tokens"),
	}
	if ct, ok := m["completion_tokens_details"].(map[string]any); ok {
		if v, ok2 := ct["reasoning_tokens"].(float64); ok2 {
			d.ReasoningTokens = int64(v)
		}
	}
	return d
}

// usageDetailFromCompletion extracts the usage block from an aggregated
// non-streaming chat.completion payload.
func usageDetailFromCompletion(payload []byte) usage.Detail {
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return usage.Detail{}
	}
	m, _ := obj["usage"].(map[string]any)
	return usageDetailFromMap(m)
}

// sseUsageCollector scans upstream SSE chunks and keeps the last "usage"
// object seen (CodeBuddy emits it on the terminal chunk).
type sseUsageCollector struct {
	last map[string]any
}

func (c *sseUsageCollector) feed(rawJSON string) {
	var chunk map[string]any
	if json.Unmarshal([]byte(rawJSON), &chunk) != nil {
		return
	}
	if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
		c.last = u
	}
}

func (c *sseUsageCollector) detail() usage.Detail {
	return usageDetailFromMap(c.last)
}
