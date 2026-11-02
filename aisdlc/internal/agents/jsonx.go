package agents

import (
	"encoding/json"
	"errors"
	"strings"
)

// ExtractJSON pulls the first top-level JSON object out of model text, tolerating
// markdown fences and chatter around it.
func ExtractJSON(s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil, errors.New("no JSON object found in model output")
	}
	raw := json.RawMessage(s[start : end+1])
	if !json.Valid(raw) {
		return nil, errors.New("model output contained invalid JSON")
	}
	return raw, nil
}
