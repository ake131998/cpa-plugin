// enterprise_refresh.go implements proactive refresh of the enterprise
// custom model list.
//
// The host only re-invokes model.for_auth on config reloads and models
// queries — if a client stays open for hours, admin-published model changes
// would never surface. A dedicated loop refreshes every enterprise's cached
// model list on a fixed cadence (enterpriseModelsTTL), independent of host
// traffic.
//
// Failure semantics: a failed refresh keeps the previously fetched list in
// the cache (stale-while-error) and records the error for the management
// status endpoint, so enterprise models never vanish on a transient upstream
// error; the next tick retries.
package main

import (
	"strings"
	"sync"
	"time"
)

var (
	enterpriseRefreshStop chan struct{}
	enterpriseRefreshMu   sync.Mutex
)

// ensureEnterpriseRefreshLoop starts the background refresh loop once.
// Idempotent: repeated configure()/register calls must not spawn duplicate
// loops. Like the checkin scheduler there is deliberately no stop path — the
// plugin shutdown export is a no-op for c-shared SIGSEGV safety.
func ensureEnterpriseRefreshLoop() {
	enterpriseRefreshMu.Lock()
	defer enterpriseRefreshMu.Unlock()
	if enterpriseRefreshStop != nil {
		return // already running
	}
	enterpriseRefreshStop = make(chan struct{})
	go enterpriseRefreshLoop(enterpriseRefreshStop)
}

func enterpriseRefreshLoop(stop chan struct{}) {
	ticker := time.NewTicker(enterpriseModelsTTL)
	defer ticker.Stop()
	// First run shortly after start so a fresh deployment doesn't wait a full
	// interval for its first fetch.
	refreshEnterpriseModelsAll()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			refreshEnterpriseModelsAll()
		}
	}
}

// refreshEnterpriseModelsAll enumerates all workbuddy auths and refreshes the
// enterprise model cache for every account that has an enterpriseId and a
// usable access token. Accounts without an enterpriseId are skipped (they
// have no enterprise list to fetch). Per-account fetches run concurrently
// with a bounded semaphore (matches runAutoCheckin's fan-out discipline).
func refreshEnterpriseModelsAll() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			refreshEnterpriseModelsForAuth(f.AuthIndex)
		}()
	}
	wg.Wait()
}

// refreshEnterpriseModelsForAuth refreshes one account's enterprise model
// cache. Failures are recorded via markEnterpriseModelsError (preserving the
// previous list) and never abort other accounts.
func refreshEnterpriseModelsForAuth(authIndex string) {
	sa, err := hostAuthGet(authIndex)
	if err != nil {
		return
	}
	enterpriseID := strings.TrimSpace(sa.Account.EnterpriseID)
	if enterpriseID == "" || strings.TrimSpace(sa.Auth.AccessToken) == "" {
		return
	}
	models, err := callEnterpriseModelsAPI(sa.Auth.AccessToken, enterpriseID)
	if err != nil || len(models) == 0 {
		markEnterpriseModelsError(enterpriseID, err)
		return
	}
	storeEnterpriseModels(enterpriseID, models)
}

// -----------------------------------------------------------------------------
// Management status endpoint: enterprise model list + refresh observability
// -----------------------------------------------------------------------------

// enterpriseStatusAccount is one account's row in the status response.
type enterpriseStatusAccount struct {
	AuthIndex     string                `json:"auth_index"`
	Nickname      string                `json:"nickname,omitempty"`
	EnterpriseID  string                `json:"enterprise_id"`
	Status        string                `json:"status"` // fresh | stale | error | pending | no_enterprise
	FetchedAt     string                `json:"fetched_at,omitempty"`
	NextRefreshAt string                `json:"next_refresh_at,omitempty"`
	LastError     string                `json:"last_error,omitempty"`
	ModelCount    int                   `json:"model_count"`
	Models        []enterpriseModelView `json:"models,omitempty"`
}

// enterpriseModelView is the basic model info exposed for display.
type enterpriseModelView struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	ContextLength       int64  `json:"context_length"`
	MaxCompletionTokens int64  `json:"max_completion_tokens"`
}

// handleEnterpriseModelsStatus returns the enterprise model cache state for
// every workbuddy account plus the refresh cadence. Read-only; mirrors the
// keepalive/status observability pattern.
func handleEnterpriseModelsStatus() map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": "host.auth.list failed: " + err.Error()}
	}
	accounts := make([]enterpriseStatusAccount, 0, len(files))
	for _, f := range files {
		sa, gerr := hostAuthGet(f.AuthIndex)
		if gerr != nil {
			continue
		}
		accounts = append(accounts, enterpriseStatusAccount{
			AuthIndex:    f.AuthIndex,
			Nickname:     sa.Account.Nickname,
			EnterpriseID: strings.TrimSpace(sa.Account.EnterpriseID),
		})
	}
	return buildEnterpriseModelsStatus(accounts)
}

// buildEnterpriseModelsStatus assembles the status rows from the cache.
// Pure function of the cache contents — unit-testable without host RPCs.
func buildEnterpriseModelsStatus(accounts []enterpriseStatusAccount) map[string]any {
	out := make([]enterpriseStatusAccount, 0, len(accounts))
	for _, a := range accounts {
		entry, ok := cachedEnterpriseModels(a.EnterpriseID)
		row := a
		if a.EnterpriseID == "" {
			row.Status = "no_enterprise"
			out = append(out, row)
			continue
		}
		if !ok {
			row.Status = "pending"
			out = append(out, row)
			continue
		}
		hasModels := len(entry.models) > 0
		switch {
		case !hasModels && entry.err != nil:
			row.Status = "error"
			row.LastError = truncateRedacted(entry.err.Error(), 200)
		case hasModels && entry.err != nil:
			row.Status = "stale" // stale-while-error: old data, last refresh failed
			row.LastError = truncateRedacted(entry.err.Error(), 200)
		case hasModels:
			row.Status = "fresh"
		default:
			row.Status = "pending"
		}
		row.ModelCount = len(entry.models)
		row.Models = make([]enterpriseModelView, 0, len(entry.models))
		for _, m := range entry.models {
			row.Models = append(row.Models, enterpriseModelView{
				ID:                  m.ID,
				Name:                m.Name,
				ContextLength:       m.ContextLength,
				MaxCompletionTokens: m.MaxCompletionTokens,
			})
		}
		if !entry.fetched.IsZero() {
			row.FetchedAt = entry.fetched.Format(time.RFC3339)
			row.NextRefreshAt = entry.fetched.Add(enterpriseModelsTTL).Format(time.RFC3339)
		}
		if entry.err != nil && row.FetchedAt == "" {
			row.NextRefreshAt = entry.errAt.Add(enterpriseModelsTTL).Format(time.RFC3339)
		}
		out = append(out, row)
	}
	return map[string]any{
		"ttl_seconds":              int(enterpriseModelsTTL / time.Second),
		"refresh_interval_seconds": int(enterpriseModelsTTL / time.Second),
		"accounts":                 out,
	}
}
