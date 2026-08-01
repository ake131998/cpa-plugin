package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDumpUpstreamResponse_WritesRawBody(t *testing.T) {
	restore := setUpstreamDumpDirForTest(t.TempDir())
	defer restore()

	body := []byte(`{"code":0,"data":{"models":[{"id":"custom:GPT"}]}}`)
	dumpUpstreamResponse("enterprise_models_ent-1", http.MethodGet, "https://up/config/models", 200, body)

	dir := loadedUpstreamDumpDir()
	raw, err := os.ReadFile(filepath.Join(dir, "enterprise_models_ent-1.json"))
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	if string(raw) != string(body) {
		t.Fatalf("raw body mismatch: %s", raw)
	}
	metaRaw, err := os.ReadFile(filepath.Join(dir, "enterprise_models_ent-1.meta.json"))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	for _, want := range []string{`"status": 200`, "https://up/config/models", `"method": "GET"`} {
		if !strings.Contains(string(metaRaw), want) {
			t.Errorf("meta missing %q: %s", want, metaRaw)
		}
	}
}

func TestDumpUpstreamResponse_DisabledByDefault(t *testing.T) {
	restore := setUpstreamDumpDirForTest("")
	defer restore()

	dir := t.TempDir()
	// Even pointing name at an existing dir must not create files when disabled.
	dumpUpstreamResponse("enterprise_models_ent-1", "GET", "https://up/x", 200, []byte(`{}`))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("dump must be no-op when disabled, found %d entries", len(entries))
	}
}

func TestDumpUpstreamResponse_SanitizesName(t *testing.T) {
	restore := setUpstreamDumpDirForTest(t.TempDir())
	defer restore()

	dumpUpstreamResponse("enterprise_models_a/b:../evil", "GET", "u", 200, []byte(`{}`))
	entries, err := os.ReadDir(loadedUpstreamDumpDir())
	if err != nil {
		t.Fatalf("read dump dir: %v", err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), "enterprise_models_") {
			found = true
			if strings.ContainsAny(e.Name(), "/:") {
				t.Errorf("unsafe file name: %s", e.Name())
			}
		}
	}
	if !found {
		t.Fatalf("no dump files written: %v", entries)
	}
}

func TestDumpUpstreamResponse_CallEnterpriseModelsIntegration(t *testing.T) {
	restore := setUpstreamDumpDirForTest(t.TempDir())
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"models":[{"id":"m1"}]}}`))
	}))
	defer srv.Close()
	old := enterpriseModelsBaseCN
	enterpriseModelsBaseCN = srv.URL + "/console/enterprises/%s/config/models"
	defer func() { enterpriseModelsBaseCN = old }()

	if _, err := callEnterpriseModelsAPI(cnToken(), "ent-int"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(loadedUpstreamDumpDir(), "enterprise_models_ent-int.json"))
	if err != nil {
		t.Fatalf("dump not written: %v", err)
	}
	if !strings.Contains(string(raw), `"id":"m1"`) {
		t.Fatalf("dump content: %s", raw)
	}
}

func TestDumpUpstreamResponse_EnrichAccountIntegration(t *testing.T) {
	restore := setUpstreamDumpDirForTest(t.TempDir())
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"uid":"u1","enterpriseId":"ent-1"}`))
	}))
	defer srv.Close()
	old := accountEndpointBaseCN
	accountEndpointBaseCN = srv.URL + "/v2/plugin/login/account"
	defer func() { accountEndpointBaseCN = old }()

	eid, _, _ := enrichAccountFromUpstream(cnToken())
	if eid != "ent-1" {
		t.Fatalf("enrich failed: %q", eid)
	}
	raw, err := os.ReadFile(filepath.Join(loadedUpstreamDumpDir(), "account_info.json"))
	if err != nil {
		t.Fatalf("dump not written: %v", err)
	}
	if !strings.Contains(string(raw), `"enterpriseId":"ent-1"`) {
		t.Fatalf("dump content: %s", raw)
	}
}
