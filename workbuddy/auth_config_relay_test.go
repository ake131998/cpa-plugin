package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Regression tests for the per-credential config wipe: CPA rewrites the auth
// file on every host-driven token refresh as StorageJSON+Metadata
// (FileTokenStore.Save → pluginTokenStorage.SaveTokenToFile →
// mergedStorageJSON). Any config key the plugin does not relay through
// AuthData.Metadata (priority / model_aliases / excluded_models / prefix)
// was silently dropped from the file on each refresh, and the watcher then
// re-synthesized an auth record without the routing attributes — aliases and
// filters "worked" until the next refresh and then vanished.

// authFileWithExtras is a physical auth file carrying every per-credential
// config key the host recognizes.
func authFileWithExtras() []byte {
	return []byte(`{
  "type": "workbuddy",
  "provider": "workbuddy",
  "priority": 3,
  "prefix": "wb",
  "model_aliases": [{"name": "kimi-k2.7", "alias": "k2"}, {"name": "glm-5.2", "alias": "g5"}],
  "excluded_models": ["hunyuan-chat", "hy3"],
  "auth": {"accessToken": "at", "refreshToken": "rt", "expiresAt": 9999999999},
  "account": {"uid": "u-extra", "nickname": "nick"}
}`)
}

func TestAuthConfigExtrasMetadata_FullFile(t *testing.T) {
	extras := authConfigExtrasMetadata(authFileWithExtras())
	if extras == nil {
		t.Fatal("expected extras")
	}
	if p, ok := extras["priority"].(int); !ok || p != 3 {
		t.Fatalf("priority: got %#v", extras["priority"])
	}
	aliases, ok := extras["model_aliases"].([]wbModelAlias)
	if !ok || len(aliases) != 2 || aliases[0].Name != "kimi-k2.7" || aliases[0].Alias != "k2" {
		t.Fatalf("model_aliases: got %#v", extras["model_aliases"])
	}
	excluded, ok := extras["excluded_models"].([]string)
	if !ok || len(excluded) != 2 || excluded[0] != "hunyuan-chat" {
		t.Fatalf("excluded_models: got %#v", extras["excluded_models"])
	}
	if prefix, ok := extras["prefix"].(string); !ok || prefix != "wb" {
		t.Fatalf("prefix: got %#v", extras["prefix"])
	}
}

func TestAuthConfigExtrasMetadata_KebabNormalizedToSnake(t *testing.T) {
	raw := []byte(`{"model-aliases": [{"name": "m1", "alias": "a1"}], "excluded-models": ["x"]}`)
	extras := authConfigExtrasMetadata(raw)
	if extras == nil {
		t.Fatal("expected extras")
	}
	if _, ok := extras["model_aliases"]; !ok {
		t.Fatalf("kebab model-aliases not normalized: %#v", extras)
	}
	if _, ok := extras["excluded_models"]; !ok {
		t.Fatalf("kebab excluded-models not normalized: %#v", extras)
	}
	if _, ok := extras["model-aliases"]; ok {
		t.Fatalf("kebab key must not leak into metadata relay: %#v", extras)
	}
}

func TestAuthConfigExtrasMetadata_NoConfig(t *testing.T) {
	if got := authConfigExtrasMetadata(sampleNestedAuthJSON("u1")); got != nil {
		t.Fatalf("no-config file must yield nil: %#v", got)
	}
	if got := authConfigExtrasMetadata([]byte("not json")); got != nil {
		t.Fatalf("invalid json must yield nil: %#v", got)
	}
	if got := authConfigExtrasMetadata(nil); got != nil {
		t.Fatalf("empty input must yield nil: %#v", got)
	}
}

func TestRelayAuthConfigExtras(t *testing.T) {
	dst := map[string]any{"type": "workbuddy", "note": "n"}
	src := map[string]any{
		"priority":        1,
		"model_aliases":   []wbModelAlias{{Name: "m", Alias: "a"}},
		"excluded_models": []string{"x"},
		"prefix":          "p",
		// non-whitelisted keys must not be relayed
		"note":  "forged",
		"other": true,
	}
	relayAuthConfigExtras(dst, src)
	for _, k := range authConfigMetadataKeys {
		if _, ok := dst[k]; !ok {
			t.Fatalf("whitelisted key %s not relayed: %#v", k, dst)
		}
	}
	if dst["note"] != "n" {
		t.Fatalf("non-whitelisted key overwritten: %#v", dst["note"])
	}
	if _, ok := dst["other"]; ok {
		t.Fatalf("non-whitelisted key relayed: %#v", dst)
	}
	// nil/empty safety: must not panic, must not touch dst.
	relayAuthConfigExtras(nil, src)
	relayAuthConfigExtras(dst, nil)
	relayAuthConfigExtras(dst, map[string]any{})
}

func TestHandleParseAuth_RelaysConfigExtrasIntoMetadata(t *testing.T) {
	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		Path:     "/root/.cli-proxy-api/workbuddy-u-extra.json",
		FileName: "workbuddy-u-extra.json",
		RawJSON:  authFileWithExtras(),
	}
	body, _ := json.Marshal(req)
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	if !resp.Handled {
		t.Fatal("expected handled")
	}
	meta := resp.Auth.Metadata
	if p, ok := meta["priority"].(float64); !ok || int(p) != 3 {
		t.Fatalf("metadata priority: got %#v", meta["priority"])
	}
	if resp.Auth.Attributes["priority"] != "3" {
		t.Fatalf("attributes priority: got %#v", resp.Auth.Attributes["priority"])
	}
	aliasesRaw, err := json.Marshal(meta["model_aliases"])
	if err != nil {
		t.Fatalf("marshal model_aliases: %v", err)
	}
	var aliases []wbModelAlias
	if err := json.Unmarshal(aliasesRaw, &aliases); err != nil || len(aliases) != 2 || aliases[0].Alias != "k2" {
		t.Fatalf("metadata model_aliases: %s (%v)", aliasesRaw, err)
	}
	excludedRaw, _ := json.Marshal(meta["excluded_models"])
	var excluded []string
	if err := json.Unmarshal(excludedRaw, &excluded); err != nil || len(excluded) != 2 {
		t.Fatalf("metadata excluded_models: %s (%v)", excludedRaw, err)
	}
	if meta["prefix"] != "wb" || resp.Auth.Prefix != "wb" {
		t.Fatalf("prefix relay: meta=%#v auth=%#v", meta["prefix"], resp.Auth.Prefix)
	}
}

func TestHandleParseAuth_NoExtrasKeepsMetadataClean(t *testing.T) {
	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		FileName: "workbuddy-u1.json",
		RawJSON:  sampleNestedAuthJSON("u1"),
	}
	body, _ := json.Marshal(req)
	out, err := handleParseAuth(body)
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	for _, k := range authConfigMetadataKeys {
		if _, ok := resp.Auth.Metadata[k]; ok {
			t.Fatalf("config-less file must not gain %s in metadata: %#v", k, resp.Auth.Metadata)
		}
	}
}

// simulateHostPersistMerge mirrors the host's mergedStorageJSON semantics
// (internal/pluginhost/auth_provider.go): decode the plugin's StorageJSON,
// overlay every Metadata key, then force "type". The result is what CPA
// writes back to the auth file on every refresh persist.
func simulateHostPersistMerge(t *testing.T, ad pluginapi.AuthData) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(ad.StorageJSON, &out); err != nil {
		t.Fatalf("storage decode: %v", err)
	}
	for k, v := range ad.Metadata {
		out[k] = v
	}
	out["type"] = providerName
	return out
}

// The core regression: a host-driven refresh must produce a file that still
// carries the per-credential config. req.Metadata stands in for the in-memory
// auth's metadata (populated by ParseAuth from the file); the refresh path
// relays its whitelisted keys into the response Metadata.
func TestRefreshResponse_PreservesConfigExtrasThroughHostPersist(t *testing.T) {
	sa, err := parseStored(sampleNestedAuthJSON("u-extra"))
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	reqMetadata := authConfigExtrasMetadata(authFileWithExtras())

	// Mirrors handleRefreshAuth's response construction.
	ad := toAuthDataForRefresh(sa)
	relayAuthConfigExtras(ad.Metadata, reqMetadata)

	file := simulateHostPersistMerge(t, ad)
	for _, k := range authConfigMetadataKeys {
		if _, ok := file[k]; !ok {
			t.Fatalf("refresh persist would drop %s from the auth file: %#v", k, file)
		}
	}
	// Sanity: the rewritten file still parses back to the same effective
	// config, so the watcher re-synthesis keeps the routing attributes.
	roundTrip, _ := json.Marshal(file)
	priority, aliases, excluded := parseAuthFileConfig(roundTrip)
	if priority == nil || *priority != 3 || len(aliases) != 2 || len(excluded) != 2 {
		t.Fatalf("effective config changed across refresh persist: p=%v a=%v e=%v", priority, aliases, excluded)
	}
	if prefix, ok := parseAuthFilePrefix(roundTrip); !ok || prefix != "wb" {
		t.Fatalf("prefix lost across refresh persist: %#v", prefix)
	}
}

// Without the relay (the old behavior) the host persist wipes the config —
// documents the bug this fix addresses.
func TestRefreshResponse_WithoutRelayDropsExtras(t *testing.T) {
	sa, err := parseStored(sampleNestedAuthJSON("u-extra"))
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	ad := toAuthDataForRefresh(sa) // no relay — pre-fix behavior
	file := simulateHostPersistMerge(t, ad)
	if _, ok := file["model_aliases"]; ok {
		t.Fatal("expected model_aliases to be wiped without relay")
	}
	if _, ok := file["excluded_models"]; ok {
		t.Fatal("expected excluded_models to be wiped without relay")
	}
}

func TestLookupExistingAuthConfigExtras_HostUnavailable(t *testing.T) {
	sa, err := parseStored(sampleNestedAuthJSON("u-none"))
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	// hostAPI is nil in unit tests: the lookup must degrade to nil without
	// panicking (login must never fail over config preservation).
	if got := lookupExistingAuthConfigExtras(sa); got != nil {
		t.Fatalf("expected nil without host, got %#v", got)
	}
	if got := lookupExistingAuthConfigExtras(nil); got != nil {
		t.Fatalf("expected nil for nil sa, got %#v", got)
	}
}
