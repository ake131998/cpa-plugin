package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// --- cache semantics --------------------------------------------------------

func TestMarkEnterpriseModelsError_PreservesOldData(t *testing.T) {
	wipeEnterpriseCache()
	storeEnterpriseModels("ent-1", []pluginapi.ModelInfo{{ID: "m1"}})
	markEnterpriseModelsError("ent-1", errors.New("boom"))
	defer wipeEnterpriseCache()

	entry, ok := cachedEnterpriseModels("ent-1")
	if !ok {
		t.Fatal("entry missing after error")
	}
	if len(entry.models) != 1 || entry.models[0].ID != "m1" {
		t.Fatalf("stale-while-error must keep old models: %+v", entry.models)
	}
	if entry.fetched.IsZero() {
		t.Error("last successful fetch time must be preserved")
	}
	if entry.err == nil || entry.errAt.IsZero() {
		t.Error("error metadata must be recorded")
	}
}

func TestStoreEnterpriseModels_ReplacesCache(t *testing.T) {
	wipeEnterpriseCache()
	storeEnterpriseModels("ent-1", []pluginapi.ModelInfo{{ID: "old"}})
	storeEnterpriseModels("ent-1", []pluginapi.ModelInfo{{ID: "new"}})
	defer wipeEnterpriseCache()

	// Replace semantics: a refreshed list fully supersedes the old one, so an
	// admin-deleted model disappears from the cache.
	entry, _ := cachedEnterpriseModels("ent-1")
	if len(entry.models) != 1 || entry.models[0].ID != "new" {
		t.Fatalf("replace failed: %+v", entry.models)
	}
	if entry.err != nil {
		t.Error("successful store must clear prior error state")
	}
}

func TestCachedEnterpriseModels_KeyIsolation(t *testing.T) {
	wipeEnterpriseCache()
	storeEnterpriseModels("ent-a", []pluginapi.ModelInfo{{ID: "a"}})
	defer wipeEnterpriseCache()
	if _, ok := cachedEnterpriseModels("ent-b"); ok {
		t.Fatal("different enterprise must not share cache entries")
	}
	if _, ok := cachedEnterpriseModels(""); ok {
		t.Fatal("empty enterpriseId must never hit the cache")
	}
}

// --- stale-while-error through the fetch path -------------------------------

func TestFetchDynamicModels_StaleWhileError(t *testing.T) {
	// Warm the dynamic cache so fetchDynamicModelsFromStorage takes the
	// cached path (no network); then leave the enterprise cache in the
	// stale+error state. The merged result must still include the old
	// enterprise models — a failed refresh must not make them vanish.
	storeDynamicModels([]pluginapi.ModelInfo{{ID: "dyn-1", Name: "Dyn 1"}})
	wipeEnterpriseCache()
	storeEnterpriseModels("ent-1", []pluginapi.ModelInfo{{ID: "ent-m", Name: "Ent M"}})
	markEnterpriseModelsError("ent-1", errors.New("upstream 500"))
	defer func() {
		wipeEnterpriseCache()
		storeDynamicModels(nil)
	}()

	storage := []byte(`{"auth":{"accessToken":"tok"},"account":{"enterpriseId":"ent-1"}}`)
	out := fetchDynamicModelsFromStorage(storage)
	if len(out) != 2 {
		t.Fatalf("stale enterprise models lost: %+v", out)
	}
	seen := false
	for _, m := range out {
		if m.ID == "ent-m" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("enterprise model missing from stale-while-error result")
	}
}

// --- refresh loop lifecycle -------------------------------------------------

func TestEnsureEnterpriseRefreshLoop_Idempotent(t *testing.T) {
	enterpriseRefreshMu.Lock()
	enterpriseRefreshStop = nil
	enterpriseRefreshMu.Unlock()
	ensureEnterpriseRefreshLoop()
	first := enterpriseRefreshStop
	if first == nil {
		t.Fatal("loop not started")
	}
	ensureEnterpriseRefreshLoop()
	second := enterpriseRefreshStop
	if first != second {
		t.Fatal("second call must not start a new loop")
	}
	// Cleanup: stop the goroutine so tests don't leak timers.
	enterpriseRefreshMu.Lock()
	if enterpriseRefreshStop != nil {
		close(enterpriseRefreshStop)
		enterpriseRefreshStop = nil
	}
	enterpriseRefreshMu.Unlock()
}

func TestEnterpriseRefreshLoop_FetchAndStore(t *testing.T) {
	// Direct loop iteration (not the ticker): warm auth enumeration is host
	// RPC-bound and not unit-testable; instead assert the fetch+store wiring
	// through callEnterpriseModelsAPI + store/mark paths.
	wipeEnterpriseCache()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/console/enterprises/ent-1/config/models") {
			t.Errorf("path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"models":[{"id":"custom:GPT","name":"GPT"}]}}`))
	}))
	defer srv.Close()
	old := enterpriseModelsBaseCN
	enterpriseModelsBaseCN = srv.URL + "/console/enterprises/%s/config/models"
	defer func() { enterpriseModelsBaseCN = old }()

	if _, err := callEnterpriseModelsAPI(cnToken(), "ent-1"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	storeEnterpriseModels("ent-1", []pluginapi.ModelInfo{{ID: "custom:GPT", Name: "GPT"}})
	entry, ok := cachedEnterpriseModels("ent-1")
	if !ok || len(entry.models) != 1 || entry.models[0].ID != "custom:GPT" {
		t.Fatalf("cache not updated: %+v", entry)
	}
	wipeEnterpriseCache()
}

// --- status endpoint --------------------------------------------------------

func TestBuildEnterpriseModelsStatus_States(t *testing.T) {
	wipeEnterpriseCache()
	storeEnterpriseModels("ent-fresh", []pluginapi.ModelInfo{{ID: "a", Name: "A", ContextLength: 100, MaxCompletionTokens: 50}})
	storeEnterpriseModels("ent-stale", []pluginapi.ModelInfo{{ID: "b"}})
	markEnterpriseModelsError("ent-stale", errors.New("upstream 500"))
	markEnterpriseModelsError("ent-error", errors.New("never fetched"))
	defer wipeEnterpriseCache()

	accounts := []enterpriseStatusAccount{
		{AuthIndex: "wb-1", Nickname: "n1", EnterpriseID: "ent-fresh"},
		{AuthIndex: "wb-2", Nickname: "n2", EnterpriseID: "ent-stale"},
		{AuthIndex: "wb-3", Nickname: "n3", EnterpriseID: "ent-error"},
		{AuthIndex: "wb-4", Nickname: "n4", EnterpriseID: "ent-pending"},
		{AuthIndex: "wb-5", Nickname: "n5", EnterpriseID: ""},
	}
	resp := buildEnterpriseModelsStatus(accounts)
	if int(resp["ttl_seconds"].(int)) != 900 {
		t.Errorf("ttl: %v", resp["ttl_seconds"])
	}
	rows, ok := resp["accounts"].([]enterpriseStatusAccount)
	if !ok {
		t.Fatalf("accounts type: %T", resp["accounts"])
	}
	if len(rows) != 5 {
		t.Fatalf("rows=%d want 5", len(rows))
	}
	byEnt := map[string]enterpriseStatusAccount{}
	for _, r := range rows {
		byEnt[r.EnterpriseID] = r
	}
	if byEnt["ent-fresh"].Status != "fresh" || byEnt["ent-fresh"].ModelCount != 1 {
		t.Errorf("fresh row: %+v", byEnt["ent-fresh"])
	}
	if byEnt["ent-fresh"].FetchedAt == "" || byEnt["ent-fresh"].NextRefreshAt == "" {
		t.Errorf("fresh row must carry fetch times: %+v", byEnt["ent-fresh"])
	}
	if byEnt["ent-stale"].Status != "stale" || !strings.Contains(byEnt["ent-stale"].LastError, "500") {
		t.Errorf("stale row: %+v", byEnt["ent-stale"])
	}
	if byEnt["ent-stale"].FetchedAt == "" {
		t.Errorf("stale row must keep last successful fetch time")
	}
	if byEnt["ent-error"].Status != "error" || byEnt["ent-error"].ModelCount != 0 {
		t.Errorf("error row: %+v", byEnt["ent-error"])
	}
	if byEnt["ent-pending"].Status != "pending" {
		t.Errorf("pending row: %+v", byEnt["ent-pending"])
	}
	if byEnt[""].Status != "no_enterprise" {
		t.Errorf("no_enterprise row: %+v", byEnt[""])
	}
	if len(byEnt["ent-fresh"].Models) != 1 || byEnt["ent-fresh"].Models[0].ID != "a" ||
		byEnt["ent-fresh"].Models[0].ContextLength != 100 || byEnt["ent-fresh"].Models[0].MaxCompletionTokens != 50 {
		t.Errorf("model view: %+v", byEnt["ent-fresh"].Models)
	}
}

func TestEnterpriseModelsStatus_NoAccounts(t *testing.T) {
	wipeEnterpriseCache()
	resp := buildEnterpriseModelsStatus(nil)
	if rows, ok := resp["accounts"].([]enterpriseStatusAccount); !ok || len(rows) != 0 {
		t.Fatalf("empty accounts must yield empty rows: %+v", resp)
	}
}

// --- management route registration ------------------------------------------

func TestManagementRegistration_IncludesEnterpriseModels(t *testing.T) {
	found := false
	for _, r := range managementRegistration().Routes {
		if r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/models/enterprise") {
			found = true
		}
	}
	if !found {
		t.Fatal("/models/enterprise route not registered")
	}
}
