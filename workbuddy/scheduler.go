// scheduler.go implements the CPA scheduler.pick capability for workbuddy.
//
// Routing uses the panel-selected active account (region from that card's
// domain). When the selection is exhausted/disabled/missing, randomly switch
// to another non-exhausted workbuddy candidate. Non-workbuddy candidates are
// always deferred so the built-in scheduler handles them.
package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Legacy config values kept for configure() compatibility; pick always uses
// panel active-auth selection now (not credit-max ranking).
const (
	schedulerModeOff     = "off"
	schedulerModeCredits = "credits"
)

var (
	schedulerMode   = schedulerModeOff
	schedulerModeMu sync.RWMutex
)

// setSchedulerMode is a test helper that returns a restore func.
func setSchedulerMode(mode string) func() {
	schedulerModeMu.Lock()
	old := schedulerMode
	schedulerMode = mode
	schedulerModeMu.Unlock()
	return func() {
		schedulerModeMu.Lock()
		schedulerMode = old
		schedulerModeMu.Unlock()
	}
}

func loadedSchedulerMode() string {
	schedulerModeMu.RLock()
	defer schedulerModeMu.RUnlock()
	return schedulerMode
}

// handleSchedulerPick selects a workbuddy auth candidate based on the
// panel-selected active account. Non-workbuddy candidates are always deferred
// (Handled: false) so the built-in scheduler handles them.
//
// scheduler_mode:
//   - "off"     → plugin does NOT handle routing; defer everything to built-in.
//   - "credits" → plugin picks via panel-selected active account (sticky, with
//     fallback when that account becomes exhausted/disabled).
//
// Default is off (see schedulerMode init). Users opting into the plugin's
// routing should set scheduler_mode: credits in plugin config.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	// v0.6.31: actually honor the scheduler_mode toggle. Previously the config
	// was parsed but never read here, so "off" silently behaved like "credits".
	if loadedSchedulerMode() != schedulerModeCredits {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Collect workbuddy candidates only.
	var wbCandidates []pluginapi.SchedulerAuthCandidate
	for _, c := range req.Candidates {
		if c.Provider != providerName {
			continue
		}
		if candidateDisabled(c) {
			continue
		}
		wbCandidates = append(wbCandidates, c)
	}
	if len(wbCandidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	// Build thin view for active-auth picker.
	buildCands := func(candidates []pluginapi.SchedulerAuthCandidate) []activeAuthCandidate {
		cands := make([]activeAuthCandidate, 0, len(candidates))
		for _, c := range candidates {
			_, exhausted := cachedCreditsScore(c.ID)
			cands = append(cands, activeAuthCandidate{
				ID:        c.ID,
				Disabled:  false, // already filtered
				Exhausted: exhausted,
			})
		}
		return cands
	}

	// Honor host priority tiers (candidate.Priority comes from the auth file's
	// top-level "priority" relayed by ParseAuth): only the highest tier
	// participates; lower tiers are tried in order when every account in the
	// tier above is exhausted. This mirrors the built-in conductor's
	// tier-then-pool semantics. Single-tier (the common no-priority case)
	// behaves exactly as before.
	tiers := make(map[int][]pluginapi.SchedulerAuthCandidate, 2)
	order := make([]int, 0, 2)
	for _, c := range wbCandidates {
		p := c.Priority
		if _, ok := tiers[p]; !ok {
			order = append(order, p)
		}
		tiers[p] = append(tiers[p], c)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(order)))
	for _, p := range order {
		cands := buildCands(tiers[p])
		usable := false
		for _, c := range cands {
			if !c.Exhausted {
				usable = true
				break
			}
		}
		if !usable {
			continue // whole tier exhausted — fall through to the next one
		}
		if picked := pickActiveAuth(cands); picked != "" {
			return okEnvelope(pluginapi.SchedulerPickResponse{
				AuthID:  picked,
				Handled: true,
			})
		}
	}
	// Every tier exhausted: preserve the legacy sticky behavior over the full
	// pool (pickActiveAuth keeps routing to some account rather than failing).
	if picked := pickActiveAuth(buildCands(wbCandidates)); picked != "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{
			AuthID:  picked,
			Handled: true,
		})
	}
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
}

// candidateDisabled reports host-disabled auth from Status/metadata.
func candidateDisabled(c pluginapi.SchedulerAuthCandidate) bool {
	st := strings.ToLower(strings.TrimSpace(c.Status))
	if st == "disabled" {
		return true
	}
	if c.Metadata != nil {
		if v, ok := c.Metadata["disabled"]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return strings.EqualFold(strings.TrimSpace(t), "true")
			}
		}
	}
	return false
}

// cachedCreditsScore returns (remain, exhausted) from accountCache.
// remain is -1 when unknown; exhausted uses isCreditsExhausted.
// Key is auth.ID (same as SchedulerAuthCandidate.ID and activeAuthID).
func cachedCreditsScore(authID string) (int64, bool) {
	v, ok := accountCache.Load(authID)
	if !ok {
		return -1, false
	}
	entry, ok := v.(*accountCacheEntry)
	if !ok || entry.credits == nil {
		return -1, false
	}
	return entry.credits.TotalRemain, isCreditsExhausted(entry.credits)
}
