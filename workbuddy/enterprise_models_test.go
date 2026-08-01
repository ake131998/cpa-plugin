package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// fakeJWT builds a three-part JWT whose payload contains the given claims.
func fakeJWT(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	raw, _ := json.Marshal(claims)
	p := base64.RawURLEncoding.EncodeToString(raw)
	return h + "." + p + ".sig"
}

func globalToken() string {
	return fakeJWT(map[string]any{"iss": "https://auth.workbuddy.ai"})
}

func cnToken() string {
	return fakeJWT(map[string]any{"iss": "https://auth.codebuddy.cn"})
}

func wipeEnterpriseCache() {
	enterpriseModelsCache.Range(func(k, _ any) bool {
		enterpriseModelsCache.Delete(k)
		return true
	})
}

// --- mergeEnterprise --------------------------------------------------------

func TestMergeEnterprise_OverrideAndAppend(t *testing.T) {
	wipeEnterpriseCache()
	storeEnterpriseModels("ent-1", []pluginapi.ModelInfo{
		{ID: "base-m2", Name: "Ent M2", ContextLength: 999, MaxCompletionTokens: 111},
		{ID: "ent-only", Name: "Ent Only", ContextLength: 888},
	})
	defer wipeEnterpriseCache()
	base := []pluginapi.ModelInfo{
		{ID: "base-m1", Name: "Base M1", ContextLength: 100},
		{ID: "BASE-M2", Name: "Base M2", ContextLength: 200}, // case-collides with ent m2
	}
	storage := `{"auth":{"accessToken":"tok"},"account":{"enterpriseId":"ent-1"}}`
	out := mergeEnterprise([]byte(storage), base)
	if len(out) != 3 {
		t.Fatalf("len=%d want 3", len(out))
	}
	if out[0].ID != "base-m1" {
		t.Errorf("first model should stay base-m1, got %s", out[0].ID)
	}
	if out[1].ID != "base-m2" || out[1].Name != "Ent M2" || out[1].ContextLength != 999 {
		t.Errorf("collision should keep base position but enterprise fields: %+v", out[1])
	}
	if out[2].ID != "ent-only" {
		t.Errorf("new enterprise model should append at end, got %s", out[2].ID)
	}
}

func TestMergeEnterprise_NoEnterpriseCache(t *testing.T) {
	wipeEnterpriseCache()
	base := []pluginapi.ModelInfo{{ID: "a"}, {ID: "b"}}
	out := mergeEnterprise([]byte(`{"auth":{"accessToken":"t"}}`), base)
	if len(out) != 2 || out[0].ID != "a" || out[1].ID != "b" {
		t.Fatalf("base must pass through untouched: %+v", out)
	}
}

func TestMergeEnterprise_NoEnterpriseID(t *testing.T) {
	wipeEnterpriseCache()
	storeEnterpriseModels("ent-x", []pluginapi.ModelInfo{{ID: "ent-only"}})
	defer wipeEnterpriseCache()
	base := []pluginapi.ModelInfo{{ID: "a"}}
	out := mergeEnterprise([]byte(`{"auth":{"accessToken":"t"},"account":{}}`), base)
	if len(out) != 1 || out[0].ID != "a" {
		t.Fatalf("account without enterpriseId must not get enterprise models: %+v", out)
	}
}

// --- parseEnterpriseModels --------------------------------------------------

func TestParseEnterpriseModels_Shapes(t *testing.T) {
	model := `{"id":"custom:GPT","name":"Custom GPT","contextWindow":131072,"maxTokens":"8192"}`
	cases := []struct {
		name string
		raw  string
	}{
		{"array", "[" + model + "]"},
		{"models key", `{"models":[` + model + `]}`},
		{"data key", `{"data":[` + model + `]}`},
		{"data.models", `{"data":{"models":[` + model + `]}}`},
		{"envelope", `{"code":0,"msg":"ok","data":{"models":[` + model + `]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := parseEnterpriseModels([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parse failed: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("len=%d want 1", len(out))
			}
			if out[0].ID != "custom:GPT" || out[0].Name != "Custom GPT" {
				t.Errorf("id/name mismatch: %+v", out[0])
			}
			if out[0].ContextLength != 131072 || out[0].MaxCompletionTokens != 8192 {
				t.Errorf("context/maxTokens mismatch: %+v", out[0])
			}
		})
	}
}

func TestParseEnterpriseModels_FieldVariants(t *testing.T) {
	// snake_case + numeric strings + missing name.
	out, err := parseEnterpriseModels([]byte(`{"models":[{"id":"m1","context_window":"4096","max_tokens":2048}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out[0].ID != "m1" || out[0].Name != "m1" || out[0].ContextLength != 4096 || out[0].MaxCompletionTokens != 2048 {
		t.Errorf("variant parse mismatch: %+v", out[0])
	}
}

func TestParseEnterpriseModels_SkipsDisabledAndInvalid(t *testing.T) {
	raw := `{"models":[
		{"id":"off","disabled":true},
		{"name":"no id"},
		{"id":"on","disabled":false}
	]}`
	out, err := parseEnterpriseModels([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(out) != 1 || out[0].ID != "on" {
		t.Fatalf("want only the enabled model, got %+v", out)
	}
}

func TestParseEnterpriseModels_Errors(t *testing.T) {
	for _, raw := range []string{
		``,
		`not json`,
		`{"other":1}`,
		`{"models":[{"id":""}]}`, // only invalid models
	} {
		if _, err := parseEnterpriseModels([]byte(raw)); err == nil {
			t.Errorf("raw=%q should error", raw)
		}
	}
}

func TestParseEnterpriseModels_EmptyListIsFresh(t *testing.T) {
	// Verified against the real upstream: an enterprise with no custom
	// models configured answers 200 with {"code":0,...,"data":[]}.
	// A well-formed empty list is a legitimate state, not an error.
	for _, raw := range []string{
		`[]`,
		`{"models":[]}`,
		`{"code":0,"msg":"OK","requestId":"x","data":[]}`,
	} {
		out, err := parseEnterpriseModels([]byte(raw))
		if err != nil {
			t.Errorf("raw=%q should parse as empty success, got err=%v", raw, err)
		}
		if len(out) != 0 {
			t.Errorf("raw=%q want empty result, got %+v", raw, out)
		}
	}
}

// --- callEnterpriseModelsAPI ------------------------------------------------

func TestCallEnterpriseModelsAPI_RealmRouting(t *testing.T) {
	cnTok := cnToken()
	glTok := globalToken()
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/console/enterprises/ent-cn/config/models") {
			t.Errorf("CN path mismatch: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+cnTok {
			t.Errorf("CN bearer mismatch: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"cn-m"}]}}`))
	}))
	defer cnSrv.Close()
	glSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/console/enterprises/ent-gl/config/models") {
			t.Errorf("Global path mismatch: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+glTok {
			t.Errorf("Global bearer mismatch: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`[{"id":"gl-m"}]`))
	}))
	defer glSrv.Close()

	oldCN, oldGL := enterpriseModelsBaseCN, enterpriseModelsBaseGlobal
	enterpriseModelsBaseCN = cnSrv.URL + "/console/enterprises/%s/config/models"
	enterpriseModelsBaseGlobal = glSrv.URL + "/console/enterprises/%s/config/models"
	defer func() {
		enterpriseModelsBaseCN, enterpriseModelsBaseGlobal = oldCN, oldGL
	}()

	cnOut, err := callEnterpriseModelsAPI(cnTok, "ent-cn")
	if err != nil || len(cnOut) != 1 || cnOut[0].ID != "cn-m" {
		t.Fatalf("CN fetch failed: %+v err=%v", cnOut, err)
	}
	glOut, err := callEnterpriseModelsAPI(glTok, "ent-gl")
	if err != nil || len(glOut) != 1 || glOut[0].ID != "gl-m" {
		t.Fatalf("Global fetch failed: %+v err=%v", glOut, err)
	}
}

func TestCallEnterpriseModelsAPI_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	old := enterpriseModelsBaseCN
	enterpriseModelsBaseCN = srv.URL + "/console/enterprises/%s/config/models"
	defer func() { enterpriseModelsBaseCN = old }()
	if _, err := callEnterpriseModelsAPI(cnToken(), "ent-x"); err == nil {
		t.Fatal("500 must error")
	}
}

// --- enterpriseClaimFromJWT -------------------------------------------------

func TestEnterpriseClaimFromJWT(t *testing.T) {
	if got, ok := enterpriseClaimFromJWT(fakeJWT(map[string]any{"iss": "x", "enterprise_id": "ent-a"})); !ok || got != "ent-a" {
		t.Errorf("enterprise_id: got %q ok=%v", got, ok)
	}
	if got, ok := enterpriseClaimFromJWT(fakeJWT(map[string]any{"orgId": 42.0})); !ok || got != "42" {
		t.Errorf("orgId number: got %q ok=%v", got, ok)
	}
	if _, ok := enterpriseClaimFromJWT(fakeJWT(map[string]any{"iss": "x"})); ok {
		t.Error("no claim should report not-ok")
	}
	if _, ok := enterpriseClaimFromJWT("not-a-jwt"); ok {
		t.Error("garbage should report not-ok")
	}
}

// --- enrichAccountFromUpstream ----------------------------------------------

func TestEnrichAccountFromUpstream_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"uid":"u1","enterpriseId":"ent-1","nickname":"nick"}`))
	}))
	defer srv.Close()
	old := accountEndpointBaseCN
	accountEndpointBaseCN = srv.URL + "/v2/plugin/login/account"
	defer func() { accountEndpointBaseCN = old }()

	eid, uid, nick := enrichAccountFromUpstream(cnToken())
	if eid != "ent-1" || uid != "u1" || nick != "nick" {
		t.Fatalf("got %q %q %q", eid, uid, nick)
	}
}

func TestEnrichAccountFromUpstream_FallbackToJWT(t *testing.T) {
	old := accountEndpointBaseCN
	accountEndpointBaseCN = "http://127.0.0.1:1/v2/plugin/login/account" // unreachable
	defer func() { accountEndpointBaseCN = old }()

	// CN token without enterprise claim → nothing.
	eid, _, _ := enrichAccountFromUpstream(cnToken())
	if eid != "" {
		t.Fatalf("no claim, no upstream → want empty, got %q", eid)
	}
	// Token carrying an enterprise claim → JWT fallback kicks in.
	tok := fakeJWT(map[string]any{"iss": "https://auth.codebuddy.cn", "tenant_id": "ent-9"})
	eid, _, _ = enrichAccountFromUpstream(tok)
	if eid != "ent-9" {
		t.Fatalf("JWT fallback failed: %q", eid)
	}
}

func TestEnrichAccountFromUpstream_EmptyToken(t *testing.T) {
	eid, uid, nick := enrichAccountFromUpstream("")
	if eid != "" || uid != "" || nick != "" {
		t.Fatal("empty token must yield empty results")
	}
}

// --- full path: handleModelForAuth with warm caches -------------------------

func TestHandleModelForAuth_Merged(t *testing.T) {
	storeDynamicModels([]pluginapi.ModelInfo{{ID: "dyn-1", Name: "Dyn 1"}, {ID: "dyn-2", Name: "Dyn 2"}})
	wipeEnterpriseCache()
	storeEnterpriseModels("ent-1", []pluginapi.ModelInfo{
		{ID: "dyn-2", Name: "Ent Override", ContextLength: 777},
		{ID: "ent-only", Name: "Ent Only"},
	})
	defer func() {
		wipeEnterpriseCache()
		storeDynamicModels(nil) // invalidate dynamic cache for other tests
	}()

	// StorageJSON is []byte: the JSON wire form is base64 (encoding/json
	// semantics for byte slices).
	req := pluginapi.AuthModelRequest{
		AuthProvider: "workbuddy",
		StorageJSON:  []byte(`{"auth":{"accessToken":"tok"},"account":{"enterpriseId":"ent-1"}}`),
	}
	reqRaw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	raw, err := handleModelForAuth(reqRaw)
	if err != nil {
		t.Fatalf("handleModelForAuth: %v", err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Provider string                `json:"provider"`
			Models   []pluginapi.ModelInfo `json:"models"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %s", raw)
	}
	if len(env.Result.Models) != 3 {
		t.Fatalf("models=%d want 3: %+v", len(env.Result.Models), env.Result.Models)
	}
	found := map[string]pluginapi.ModelInfo{}
	for _, m := range env.Result.Models {
		found[m.ID] = m
	}
	if found["dyn-2"].Name != "Ent Override" || found["dyn-2"].ContextLength != 777 {
		t.Errorf("enterprise must override dyn-2: %+v", found["dyn-2"])
	}
	if _, ok := found["ent-only"]; !ok {
		t.Error("ent-only missing")
	}
}
