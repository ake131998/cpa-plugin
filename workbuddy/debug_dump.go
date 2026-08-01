// debug_dump.go writes raw upstream responses to disk for response-shape
// verification.
//
// Some upstream endpoints have a non-contractual JSON shape (the enterprise
// config/models list, the stateless login/account lookup) that cannot be
// probed with curl — they require an OAuth bearer token that only this plugin
// holds after a login. The plugin therefore mirrors the raw body it receives
// to <dump-dir>/<name>.json (plus a .meta.json with request metadata) so the
// real response shape can be confirmed by hand.
//
// Control: default disabled. Set WB_UPSTREAM_DUMP_DIR to a writable directory
// to enable. All failures are silent — dumping must never affect normal
// operation.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	upstreamDumpDirMu   sync.RWMutex
	upstreamDumpDir     string // "" = disabled
	upstreamDumpDirOnce sync.Once
)

// loadedUpstreamDumpDir returns the dump directory, reading the
// WB_UPSTREAM_DUMP_DIR env once at first use. Empty means disabled.
func loadedUpstreamDumpDir() string {
	upstreamDumpDirOnce.Do(func() {
		upstreamDumpDirMu.Lock()
		upstreamDumpDir = strings.TrimSpace(os.Getenv("WB_UPSTREAM_DUMP_DIR"))
		upstreamDumpDirMu.Unlock()
	})
	upstreamDumpDirMu.RLock()
	defer upstreamDumpDirMu.RUnlock()
	return upstreamDumpDir
}

// setUpstreamDumpDirForTest overrides the dump dir and returns a restore func.
func setUpstreamDumpDirForTest(dir string) func() {
	upstreamDumpDirOnce.Do(func() {}) // env already resolved; use override
	upstreamDumpDirMu.Lock()
	old := upstreamDumpDir
	upstreamDumpDir = dir
	upstreamDumpDirMu.Unlock()
	return func() {
		upstreamDumpDirMu.Lock()
		upstreamDumpDir = old
		upstreamDumpDirMu.Unlock()
	}
}

// dumpUpstreamResponse writes the raw body plus request metadata of one
// upstream response. name is used for the file name (sanitized); url/status
// are recorded in the meta file. No-op when dumping is disabled.
func dumpUpstreamResponse(name, method, url string, status int, body []byte) {
	dir := loadedUpstreamDumpDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	safe := sanitizeUIDForFileName(name)
	if safe == "" {
		safe = "response"
	}
	meta, err := json.MarshalIndent(map[string]any{
		"method":   method,
		"url":      url,
		"status":   status,
		"received": time.Now().Format(time.RFC3339),
		"bytes":    len(body),
	}, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, safe+".meta.json"), meta, 0o644)
	if len(body) > 0 {
		_ = os.WriteFile(filepath.Join(dir, safe+".json"), body, 0o644)
	}
}
