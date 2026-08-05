package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// --- readAuthFileExtras -------------------------------------------------

func TestReadAuthFileExtras_ExtractsWhitelist(t *testing.T) {
	raw := []byte(`{
		"type": "workbuddy",
		"provider": "workbuddy",
		"disabled": true,
		"note": "operator note",
		"priority": 10,
		"model_aliases": [{"name":"kimi-k2.7","alias":"k2"}],
		"excluded_models": ["glm-5v-turbo"],
		"prefix": "wb",
		"auth": {"accessToken": "at"},
		"account": {"uid": "u"}
	}`)
	extras := readAuthFileExtras(raw)
	if extras == nil {
		t.Fatal("expected extras")
	}
	if p, ok := extras["priority"].(float64); !ok || p != 10 {
		t.Fatalf("priority=%v", extras["priority"])
	}
	if _, ok := extras["model_aliases"]; !ok {
		t.Fatal("model_aliases missing")
	}
	if _, ok := extras["excluded_models"]; !ok {
		t.Fatal("excluded_models missing")
	}
	if _, ok := extras["prefix"]; !ok {
		t.Fatal("prefix missing")
	}
	// Plugin-managed keys must never ride along (they would override the
	// caller-computed values in buildAuthFileJSON).
	for _, k := range []string{"type", "provider", "disabled", "note", "auth", "account", "logo"} {
		if _, ok := extras[k]; ok {
			t.Fatalf("managed key %q leaked into extras", k)
		}
	}
}

func TestReadAuthFileExtras_NoneAndInvalid(t *testing.T) {
	if got := readAuthFileExtras([]byte(`{"type":"workbuddy"}`)); got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	if got := readAuthFileExtras([]byte(`not json`)); got != nil {
		t.Fatalf("expected nil for invalid JSON, got %v", got)
	}
	if got := readAuthFileExtras(nil); got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}
}

// --- parseAuthFilePriority ----------------------------------------------

func TestParseAuthFilePriority(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
		ok   bool
	}{
		{"number", `{"priority": 10}`, 10, true},
		{"zero", `{"priority": 0}`, 0, true},
		{"negative", `{"priority": -3}`, -3, true},
		{"string", `{"priority": "7"}`, 7, true},
		{"absent", `{"type":"workbuddy"}`, 0, false},
		{"bad string", `{"priority": "abc"}`, 0, false},
		{"null", `{"priority": null}`, 0, false},
		{"invalid json", `nope`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseAuthFilePriority([]byte(tc.raw))
			if ok != tc.ok || (ok && got != tc.want) {
				t.Fatalf("got (%d,%v) want (%d,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// --- parseAuthFileConfig -------------------------------------------------

func TestParseAuthFileConfig_SnakeAndKebab(t *testing.T) {
	snake := []byte(`{"priority":5,"model_aliases":[{"name":"kimi-k2.7","alias":"k2"}],"excluded_models":["a","b"]}`)
	p, aliases, excluded := parseAuthFileConfig(snake)
	if p == nil || *p != 5 {
		t.Fatalf("priority=%v", p)
	}
	if len(aliases) != 1 || aliases[0].Name != "kimi-k2.7" || aliases[0].Alias != "k2" {
		t.Fatalf("aliases=%+v", aliases)
	}
	if len(excluded) != 2 || excluded[0] != "a" {
		t.Fatalf("excluded=%v", excluded)
	}

	kebab := []byte(`{"model-aliases":[{"name":"glm-5.2","alias":"glm"}],"excluded-models":["x"]}`)
	p, aliases, excluded = parseAuthFileConfig(kebab)
	if p != nil {
		t.Fatalf("priority should be nil, got %v", *p)
	}
	if len(aliases) != 1 || aliases[0].Alias != "glm" {
		t.Fatalf("kebab aliases=%+v", aliases)
	}
	if len(excluded) != 1 || excluded[0] != "x" {
		t.Fatalf("kebab excluded=%v", excluded)
	}
}

func TestParseAuthFileConfig_DropsEmptyEntries(t *testing.T) {
	raw := []byte(`{"model_aliases":[{"name":"","alias":"k2"},{"name":"n","alias":"a"}],"excluded_models":["","ok"]}`)
	_, aliases, excluded := parseAuthFileConfig(raw)
	if len(aliases) != 1 || aliases[0].Alias != "a" {
		t.Fatalf("aliases=%+v", aliases)
	}
	if len(excluded) != 1 || excluded[0] != "ok" {
		t.Fatalf("excluded=%v", excluded)
	}
}

// --- buildAuthFileJSON + extras round trip -------------------------------

func TestBuildAuthFileJSON_CarriesExtras(t *testing.T) {
	sa := &storedAuth{}
	sa.Auth.AccessToken = "at"
	sa.Auth.RefreshToken = "rt"
	sa.Auth.Domain = "www.codebuddy.cn"
	sa.Account.UID = "u1"
	sa.Account.Nickname = "nick"
	extras := map[string]any{
		"priority":        10,
		"model_aliases":   []wbModelAlias{{Name: "kimi-k2.7", Alias: "k2"}},
		"excluded_models": []string{"glm-5v-turbo"},
	}
	raw, err := buildAuthFileJSON(sa, false, "note", extras)
	if err != nil {
		t.Fatal(err)
	}
	// The rewritten file must re-parse to the same effective config.
	p, aliases, excluded := parseAuthFileConfig(raw)
	if p == nil || *p != 10 {
		t.Fatalf("priority=%v", p)
	}
	if len(aliases) != 1 || aliases[0].Alias != "k2" {
		t.Fatalf("aliases=%+v", aliases)
	}
	if len(excluded) != 1 || excluded[0] != "glm-5v-turbo" {
		t.Fatalf("excluded=%v", excluded)
	}
	// Managed keys come from the call args, not from extras.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["type"] != providerName || doc["note"] != "note" || doc["disabled"] != false {
		t.Fatalf("managed keys wrong: %v %v %v", doc["type"], doc["note"], doc["disabled"])
	}
}

// --- handleParseAuth priority passthrough --------------------------------

func TestHandleParseAuth_RelaysPriority(t *testing.T) {
	uid := "00e26541-1884-4916-9c26-253a325d64ac"
	raw := sampleNestedAuthJSON(uid)
	// Inject a top-level priority into the sample file.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["priority"] = 10
	withPriority, _ := json.Marshal(doc)

	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		FileName: "workbuddy-" + uid + ".json",
		RawJSON:  withPriority,
	}
	out, err := handleParseAuth(mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	if !resp.Handled {
		t.Fatal("should handle own file")
	}
	if resp.Auth.Attributes["priority"] != "10" {
		t.Fatalf("attributes priority=%q", resp.Auth.Attributes["priority"])
	}
	if p, ok := resp.Auth.Metadata["priority"].(float64); !ok || p != 10 {
		t.Fatalf("metadata priority=%v", resp.Auth.Metadata["priority"])
	}
}

func TestHandleParseAuth_NoPriority_LeavesAttributesEmpty(t *testing.T) {
	uid := "00e26541-1884-4916-9c26-253a325d64ac"
	req := pluginapi.AuthParseRequest{
		Provider: providerName,
		FileName: "workbuddy-" + uid + ".json",
		RawJSON:  sampleNestedAuthJSON(uid),
	}
	out, err := handleParseAuth(mustMarshal(t, req))
	if err != nil {
		t.Fatal(err)
	}
	resp := decodeParseAuth(t, out)
	if !resp.Handled {
		t.Fatal("should handle own file")
	}
	if _, ok := resp.Auth.Attributes["priority"]; ok {
		t.Fatal("priority attribute should be absent when the file has none")
	}
	if _, ok := resp.Auth.Metadata["priority"]; ok {
		t.Fatal("priority metadata should be absent when the file has none")
	}
}

// --- scheduler priority tiers ---------------------------------------------

func TestSchedulerPick_HigherTierWins(t *testing.T) {
	resetActiveAuth(t)
	accountCache.Store("wb-low", &accountCacheEntry{credits: &creditsSummary{TotalRemain: 100, TotalSize: 100}})
	accountCache.Store("wb-high", &accountCacheEntry{credits: &creditsSummary{TotalRemain: 50, TotalSize: 50}})
	t.Cleanup(func() {
		accountCache.Delete("wb-low")
		accountCache.Delete("wb-high")
	})
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-low", Provider: providerName, Priority: 0},
			{ID: "wb-high", Provider: providerName, Priority: 10},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-high" {
		t.Fatalf("expected wb-high, got %+v", resp)
	}
}

func TestSchedulerPick_ExhaustedTopTier_FallsThrough(t *testing.T) {
	resetActiveAuth(t)
	accountCache.Store("wb-high", &accountCacheEntry{credits: &creditsSummary{TotalRemain: 0, TotalUsed: 100, TotalSize: 100}})
	accountCache.Store("wb-low", &accountCacheEntry{credits: &creditsSummary{TotalRemain: 100, TotalSize: 100}})
	t.Cleanup(func() {
		accountCache.Delete("wb-high")
		accountCache.Delete("wb-low")
	})
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-high", Provider: providerName, Priority: 10},
			{ID: "wb-low", Provider: providerName, Priority: 0},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	if !resp.Handled || resp.AuthID != "wb-low" {
		t.Fatalf("expected fallback to wb-low, got %+v", resp)
	}
}

func TestSchedulerPick_AllTiersExhausted_StaysSticky(t *testing.T) {
	resetActiveAuth(t)
	accountCache.Store("wb-high", &accountCacheEntry{credits: &creditsSummary{TotalRemain: 0, TotalUsed: 100, TotalSize: 100}})
	accountCache.Store("wb-low", &accountCacheEntry{credits: &creditsSummary{TotalRemain: 0, TotalUsed: 50, TotalSize: 50}})
	t.Cleanup(func() {
		accountCache.Delete("wb-high")
		accountCache.Delete("wb-low")
	})
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-high", Provider: providerName, Priority: 10},
			{ID: "wb-low", Provider: providerName, Priority: 0},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	// Legacy behavior: all exhausted still routes somewhere instead of failing.
	if !resp.Handled || resp.AuthID == "" {
		t.Fatalf("expected sticky pick, got %+v", resp)
	}
}

func TestSchedulerPick_SameTier_KeepsActiveSelection(t *testing.T) {
	resetActiveAuth(t)
	setActiveAuthID("wb-b")
	accountCache.Store("wb-a", &accountCacheEntry{credits: &creditsSummary{TotalRemain: 100, TotalSize: 100}})
	accountCache.Store("wb-b", &accountCacheEntry{credits: &creditsSummary{TotalRemain: 100, TotalSize: 100}})
	t.Cleanup(func() {
		accountCache.Delete("wb-a")
		accountCache.Delete("wb-b")
	})
	raw, err := handleSchedulerPick(mustMarshal(t, pluginapi.SchedulerPickRequest{
		Provider: providerName,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "wb-a", Provider: providerName},
			{ID: "wb-b", Provider: providerName},
		},
	}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	resp := parsePickResponse(t, raw)
	// Single tier (all priority 0): panel active selection must be kept.
	if !resp.Handled || resp.AuthID != "wb-b" {
		t.Fatalf("expected sticky wb-b, got %+v", resp)
	}
}

// --- mergeAuthFileConfig ---------------------------------------------------

func mergeBody(t *testing.T, jsonBody string) accountConfigRequest {
	t.Helper()
	var body accountConfigRequest
	if err := json.Unmarshal([]byte(jsonBody), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	return body
}

func baseDoc(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(sampleNestedAuthJSON("u1"), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestMergeAuthFileConfig_SetAndKeep(t *testing.T) {
	doc := baseDoc(t)
	doc["note"] = "keep me"
	body := mergeBody(t, `{"auth_index":"x","priority":10,"model_aliases":[{"name":"kimi-k2.7","alias":"k2"}],"excluded_models":["glm-5v-turbo"]}`)
	if err := mergeAuthFileConfig(doc, body); err != nil {
		t.Fatal(err)
	}
	if doc["priority"] != 10 {
		t.Fatalf("priority=%v", doc["priority"])
	}
	if doc["note"] != "keep me" {
		t.Fatal("unrelated key clobbered")
	}
	aliases, _ := doc["model_aliases"].([]wbModelAlias)
	if len(aliases) != 1 || aliases[0].Alias != "k2" {
		t.Fatalf("aliases=%+v", doc["model_aliases"])
	}
	excluded, _ := doc["excluded_models"].([]string)
	if len(excluded) != 1 || excluded[0] != "glm-5v-turbo" {
		t.Fatalf("excluded=%v", doc["excluded_models"])
	}
	// Round-trip: the merged doc must serialize and re-parse cleanly.
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p, pa, pe := parseAuthFileConfig(raw)
	if p == nil || *p != 10 || len(pa) != 1 || len(pe) != 1 {
		t.Fatalf("round trip: %v %+v %v", p, pa, pe)
	}
}

func TestMergeAuthFileConfig_NullClears(t *testing.T) {
	doc := baseDoc(t)
	doc["priority"] = 5
	doc["model-aliases"] = []wbModelAlias{{Name: "n", Alias: "a"}}
	doc["excluded-models"] = []string{"x"}
	body := mergeBody(t, `{"auth_index":"x","priority":null,"model_aliases":null,"excluded_models":null}`)
	if err := mergeAuthFileConfig(doc, body); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"priority", "model_aliases", "model-aliases", "excluded_models", "excluded-models"} {
		if _, ok := doc[k]; ok {
			t.Fatalf("key %q should be cleared", k)
		}
	}
}

func TestMergeAuthFileConfig_AbsentFieldsUntouched(t *testing.T) {
	doc := baseDoc(t)
	doc["priority"] = 5
	body := mergeBody(t, `{"auth_index":"x","excluded_models":["a"]}`)
	if err := mergeAuthFileConfig(doc, body); err != nil {
		t.Fatal(err)
	}
	if doc["priority"] != 5 {
		t.Fatal("absent priority should be kept")
	}
}

func TestMergeAuthFileConfig_Validation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"fractional priority", `{"priority":1.5}`},
		{"bool priority", `{"priority":true}`},
		{"bad alias shape", `{"model_aliases":{"a":"b"}}`},
		{"alias missing name", `{"model_aliases":[{"alias":"x"}]}`},
		{"excluded wrong type", `{"excluded_models":"glm"}`},
		{"excluded empty entry", `{"excluded_models":[""]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := baseDoc(t)
			if err := mergeAuthFileConfig(doc, mergeBody(t, tc.body)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestMergeAuthFileConfig_StringPriorityAccepted(t *testing.T) {
	doc := baseDoc(t)
	body := mergeBody(t, `{"priority":"8"}`)
	if err := mergeAuthFileConfig(doc, body); err != nil {
		t.Fatal(err)
	}
	if doc["priority"] != 8 {
		t.Fatalf("priority=%v", doc["priority"])
	}
}

// Kebab-spelled config in an existing file is normalized to snake on the
// next save via the panel route (host accepts both; one spelling on disk
// keeps the file unambiguous).
func TestMergeAuthFileConfig_SetReplacesKebab(t *testing.T) {
	doc := baseDoc(t)
	doc["model-aliases"] = []wbModelAlias{{Name: "old", Alias: "o"}}
	body := mergeBody(t, `{"model_aliases":[{"name":"n","alias":"a"}]}`)
	if err := mergeAuthFileConfig(doc, body); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["model-aliases"]; ok {
		t.Fatal("kebab key should be removed when snake is set")
	}
	if _, ok := doc["model_aliases"]; !ok {
		t.Fatal("snake key missing")
	}
	if !strings.Contains(string(mustMarshal(t, doc)), `"model_aliases"`) {
		t.Fatal("serialized doc should use snake key")
	}
}
