package main

import (
	"encoding/json"
	"testing"
)

// decodeHostHTTPResponse must tolerate BOTH wire shapes: the host serializes
// pluginapi.HTTPResponse (no json tags → PascalCase "StatusCode"/"Headers"/
// "Body"), while older builds wrote snake_case "status_code"/"headers"/"body".
func TestDecodeHostHTTPResponse_PascalCaseWire(t *testing.T) {
	// Exact wire bytes as produced by marshalRPCResult(pluginapi.HTTPResponse)
	// — Go field names, no json tags.
	raw := json.RawMessage(`{"StatusCode":200,"Headers":{"X-Request-Id":["r1"]},"Body":"aGVsbG8="}`)
	resp, err := decodeHostHTTPResponse(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode=%d want 200", resp.StatusCode)
	}
	if got := resp.Headers.Get("X-Request-Id"); got != "r1" {
		t.Errorf("X-Request-Id=%q want r1", got)
	}
	if string(resp.Body) != "hello" {
		t.Errorf("body=%q want hello", resp.Body)
	}
}

func TestDecodeHostHTTPResponse_SnakeCaseWire(t *testing.T) {
	raw := json.RawMessage(`{"status_code":201,"headers":{"X-Request-Id":["r2"]},"body":"aGVsbG8y"}`)
	resp, err := decodeHostHTTPResponse(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != 201 {
		t.Errorf("StatusCode=%d want 201", resp.StatusCode)
	}
	if got := resp.Headers.Get("X-Request-Id"); got != "r2" {
		t.Errorf("X-Request-Id=%q want r2", got)
	}
	if string(resp.Body) != "hello2" {
		t.Errorf("body=%q want hello2", resp.Body)
	}
}

func TestDecodeHostHTTPResponse_MixedWire(t *testing.T) {
	// PascalCase status/body with snake_case (empty) fields present — the
	// merge path must pick up the PascalCase body when the snake one is
	// absent.
	raw := json.RawMessage(`{"status_code":0,"headers":null,"body":null,"StatusCode":302,"Headers":{"L":["1"]},"Body":"bWl4ZWQ="}`)
	resp, err := decodeHostHTTPResponse(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusCode != 302 {
		t.Errorf("StatusCode=%d want 302", resp.StatusCode)
	}
	if got := resp.Headers.Get("L"); got != "1" {
		t.Errorf("L=%q want 1", got)
	}
	if string(resp.Body) != "mixed" {
		t.Errorf("body=%q want mixed", resp.Body)
	}
}

func TestDecodeHostHTTPResponse_NonJSON(t *testing.T) {
	if _, err := decodeHostHTTPResponse(json.RawMessage(`not json`)); err == nil {
		t.Fatal("garbage must error")
	}
}
