package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// --- callModelsAPI -----------------------------------------------------------

func TestCallModelsAPI_NoAgents_FullListWithChatFilter(t *testing.T) {
	// The live upstream no longer sends an agents array; the full model list
	// must be used, chat-tagged only, with the real maxInputTokens/
	// maxOutputTokens field names (numeric and numeric-string).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+cnToken() {
			t.Errorf("bearer mismatch: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
			{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":1000000,"maxOutputTokens":8192,"tags":["chat"]},
			{"id":"kimi-k3-2","maxInputTokens":"1000000","maxOutputTokens":"32768","tags":["chat"]},
			{"id":"untagged"},
			{"id":"hunyuan-image-v3.0-art","tags":["image","art"]},
			{"id":"disabled-one","disabled":true,"tags":["chat"]}
		]}}`))
	}))
	defer srv.Close()
	old := personalModelsBaseCN
	personalModelsBaseCN = srv.URL + "/console/enterprises/personal/models"
	defer func() { personalModelsBaseCN = old }()

	out, err := callModelsAPI(cnToken())
	if err != nil {
		t.Fatalf("callModelsAPI: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len=%d want 3 (glm-5.2, kimi-k3-2, untagged): %+v", len(out), out)
	}
	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range out {
		byID[m.ID] = m
	}
	if m := byID["glm-5.2"]; m.ContextLength != 1000000 || m.MaxCompletionTokens != 8192 {
		t.Errorf("glm-5.2 fields: %+v", m)
	}
	if m := byID["kimi-k3-2"]; m.ContextLength != 1000000 || m.MaxCompletionTokens != 32768 {
		t.Errorf("kimi-k3-2 numeric-string fields: %+v", m)
	}
	if _, ok := byID["hunyuan-image-v3.0-art"]; ok {
		t.Error("non-chat tagged model must be dropped")
	}
	if _, ok := byID["disabled-one"]; ok {
		t.Error("disabled model must be dropped")
	}
}

func TestCallModelsAPI_CliAgentPreferred(t *testing.T) {
	// When the legacy agents[].name=="cli" contract is present, only the cli
	// agent's model ids are served (backwards compatibility), and cli order
	// wins over response order.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{
			"models":[
				{"id":"a","contextWindow":131072,"maxTokens":4096},
				{"id":"b","tags":["chat"]},
				{"id":"c","tags":["chat"]}
			],
			"agents":[{"name":"cli","models":["c","a"]},{"name":"web","models":["b"]}]
		}}`))
	}))
	defer srv.Close()
	old := personalModelsBaseCN
	personalModelsBaseCN = srv.URL + "/console/enterprises/personal/models"
	defer func() { personalModelsBaseCN = old }()

	out, err := callModelsAPI(cnToken())
	if err != nil {
		t.Fatalf("callModelsAPI: %v", err)
	}
	if len(out) != 2 || out[0].ID != "c" || out[1].ID != "a" {
		t.Fatalf("want cli-ordered [c a], got %+v", out)
	}
	if out[1].ContextLength != 131072 || out[1].MaxCompletionTokens != 4096 {
		t.Errorf("legacy contextWindow/maxTokens fields: %+v", out[1])
	}
}

func TestCallModelsAPI_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := personalModelsBaseCN
	personalModelsBaseCN = srv.URL + "/console/enterprises/personal/models"
	defer func() { personalModelsBaseCN = old }()
	if _, err := callModelsAPI(cnToken()); err == nil {
		t.Fatal("500 must error")
	}
}

func TestCallModelsAPI_EmptyUsableList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
	}))
	defer srv.Close()
	old := personalModelsBaseCN
	personalModelsBaseCN = srv.URL + "/console/enterprises/personal/models"
	defer func() { personalModelsBaseCN = old }()
	if _, err := callModelsAPI(cnToken()); err == nil {
		t.Fatal("empty usable list must error so callers fall back")
	}
}

func TestCallModelsAPI_GlobalRealm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"g1","tags":["chat"]}]}}`))
	}))
	defer srv.Close()
	old := personalModelsBaseGlobal
	personalModelsBaseGlobal = srv.URL + "/console/enterprises/personal/models"
	defer func() { personalModelsBaseGlobal = old }()
	out, err := callModelsAPI(globalToken())
	if err != nil || len(out) != 1 || out[0].ID != "g1" {
		t.Fatalf("global realm fetch failed: %+v err=%v", out, err)
	}
}

// --- wbModels fallback list --------------------------------------------------

func TestWBModels_RealFallbackList(t *testing.T) {
	ids := map[string]bool{}
	for _, m := range wbModels() {
		ids[m.ID] = true
	}
	for _, want := range []string{
		"default", "kimi-k3-2", "kimi-k2.7", "minimax-m3-pay", "deepseek-v4-flash",
		"glm-5.2", "hunyuan-chat", "hy3",
	} {
		if !ids[want] {
			t.Errorf("fallback list missing %q", want)
		}
	}
	for _, banned := range []string{
		"minimax-m3", "hy3-preview", "hy3-preview-agent", "hunyuan-image-v3.0-art",
	} {
		if ids[banned] {
			t.Errorf("fallback list must not contain %q", banned)
		}
	}
	for _, m := range wbModels() {
		if m.ID == "kimi-k3-2" && (m.ContextLength != 1000000 || m.MaxCompletionTokens != 32768) {
			t.Errorf("kimi-k3-2 verified spec not reflected: %+v", m)
		}
	}
}

// --- handleModelStatic -------------------------------------------------------

func TestHandleModelStatic_Empty(t *testing.T) {
	// Tokenless (static/global) path must serve an empty list — the real
	// model set is per-auth; a hardcoded global list leaks stale models.
	req := pluginapi.StaticModelRequest{}
	reqRaw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw, err := handleModelStatic(reqRaw)
	if err != nil {
		t.Fatalf("handleModelStatic: %v", err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Provider string                `json:"provider"`
			Models   []pluginapi.ModelInfo `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	if env.Result.Provider != providerName {
		t.Errorf("provider=%q want %q", env.Result.Provider, providerName)
	}
	if len(env.Result.Models) != 0 {
		t.Errorf("static path must serve empty list, got %+v", env.Result.Models)
	}
}
