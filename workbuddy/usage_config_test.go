package main

import (
	"encoding/json"
	"testing"
)

// TestConfigureEnterpriseLogging verifies the enterprise_logging config flag:
// off by default, on only when explicitly set true.
func TestConfigureEnterpriseLogging(t *testing.T) {
	// Default: absent from config_yaml → false.
	configure(mustConfigYAML(t, ""))
	if enterpriseLoggingEnabled() {
		t.Fatal("default must be off")
	}

	configure(mustConfigYAML(t, "enterprise_logging: true"))
	if !enterpriseLoggingEnabled() {
		t.Fatal("enterprise_logging: true must enable logging")
	}

	configure(mustConfigYAML(t, "enterprise_logging: false"))
	if enterpriseLoggingEnabled() {
		t.Fatal("enterprise_logging: false must disable logging")
	}

	// Quoted values are tolerated like the other boolean fields.
	configure(mustConfigYAML(t, `enterprise_logging: "on"`))
	if !enterpriseLoggingEnabled() {
		t.Fatal("quoted on must enable logging")
	}

	configure(mustConfigYAML(t, "")) // reset for other tests
}

func mustConfigYAML(t *testing.T, yaml string) []byte {
	t.Helper()
	// []byte marshals as base64 over the wire — build the request struct
	// directly so the round-trip preserves the raw YAML text.
	raw, err := json.Marshal(struct {
		ConfigYAML []byte `json:"config_yaml"`
	}{ConfigYAML: []byte(yaml)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}
