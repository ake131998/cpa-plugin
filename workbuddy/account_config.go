// account_config.go implements the per-credential config endpoint
// (POST /plugins/workbuddy/accounts/config): priority (host scheduling tier),
// model_aliases, and excluded_models — the host-recognized top-level auth
// file keys (synthesizer/file.go). Fields absent from the request body are
// left untouched; an explicit JSON null clears that key.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// accountConfigRequest uses RawMessage per field so "absent" (keep) and
// explicit null (clear) are distinguishable.
type accountConfigRequest struct {
	AuthIndex      string          `json:"auth_index"`
	Priority       json.RawMessage `json:"priority"`
	ModelAliases   json.RawMessage `json:"model_aliases"`
	ExcludedModels json.RawMessage `json:"excluded_models"`
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// mergeAuthFileConfig applies one accountConfigRequest to the decoded auth
// file document in place. Pure — the handler owns all host RPC.
func mergeAuthFileConfig(doc map[string]any, body accountConfigRequest) error {
	if len(body.Priority) > 0 {
		if isJSONNull(body.Priority) {
			delete(doc, "priority")
		} else {
			var p int
			dec := json.NewDecoder(strings.NewReader(string(body.Priority)))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				return fmt.Errorf("priority: invalid JSON")
			}
			switch t := v.(type) {
			case json.Number:
				n, err := t.Int64()
				if err != nil {
					return fmt.Errorf("priority: must be an integer")
				}
				p = int(n)
			case string:
				n, err := strconv.Atoi(strings.TrimSpace(t))
				if err != nil {
					return fmt.Errorf("priority: must be an integer")
				}
				p = n
			default:
				return fmt.Errorf("priority: must be an integer or null")
			}
			doc["priority"] = p
		}
	}
	if len(body.ModelAliases) > 0 {
		if isJSONNull(body.ModelAliases) {
			delete(doc, "model_aliases")
			delete(doc, "model-aliases")
		} else {
			var list []wbModelAlias
			if err := json.Unmarshal(body.ModelAliases, &list); err != nil {
				return fmt.Errorf("model_aliases: must be an array of {name, alias}")
			}
			out := make([]wbModelAlias, 0, len(list))
			for i, e := range list {
				e.Name = strings.TrimSpace(e.Name)
				e.Alias = strings.TrimSpace(e.Alias)
				if e.Name == "" || e.Alias == "" {
					return fmt.Errorf("model_aliases[%d]: name and alias are required", i)
				}
				out = append(out, e)
			}
			doc["model_aliases"] = out
			delete(doc, "model-aliases") // normalize to one spelling on disk
		}
	}
	if len(body.ExcludedModels) > 0 {
		if isJSONNull(body.ExcludedModels) {
			delete(doc, "excluded_models")
			delete(doc, "excluded-models")
		} else {
			var list []string
			if err := json.Unmarshal(body.ExcludedModels, &list); err != nil {
				return fmt.Errorf("excluded_models: must be an array of model ids")
			}
			out := make([]string, 0, len(list))
			for i, s := range list {
				s = strings.TrimSpace(s)
				if s == "" {
					return fmt.Errorf("excluded_models[%d]: empty model id", i)
				}
				out = append(out, s)
			}
			doc["excluded_models"] = out
			delete(doc, "excluded-models") // normalize to one spelling on disk
		}
	}
	return nil
}

// handleAccountConfig merges per-credential config into the physical auth
// file and saves it back through the host (the watcher then re-parses it,
// which is what makes priority / aliases take effect).
func handleAccountConfig(req pluginapi.ManagementRequest) map[string]any {
	var body accountConfigRequest
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"success": false, "error": "invalid JSON body"}
	}
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"success": false, "error": "auth_index is required"}
	}
	if len(body.Priority) == 0 && len(body.ModelAliases) == 0 && len(body.ExcludedModels) == 0 {
		return map[string]any{"success": false, "error": "nothing to update: pass priority, model_aliases and/or excluded_models"}
	}
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return map[string]any{"success": false, "error": "load auth: " + err.Error()}
	}
	var doc map[string]any
	if err := json.Unmarshal(phys.JSON, &doc); err != nil {
		return map[string]any{"success": false, "error": "auth file is not valid JSON"}
	}
	if err := mergeAuthFileConfig(doc, body); err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	name := phys.Name
	if name == "" {
		if sa, serr := hostAuthGet(authIndex); serr == nil {
			name = authFileNameFor(sa)
		}
	}
	if err := hostAuthSaveJSON(name, raw); err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	priority, aliases, excluded := parseAuthFileConfig(raw)
	return map[string]any{
		"success":         true,
		"auth_index":      authIndex,
		"priority":        priority,
		"model_aliases":   aliases,
		"excluded_models": excluded,
	}
}
