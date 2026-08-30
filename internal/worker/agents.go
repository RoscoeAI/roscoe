package worker

import (
	"encoding/json"
	"fmt"

	"roscoe.sh/roscoe/internal/config"
)

// agentJSON is the shape claude's --agents flag consumes, keyed by agent name.
type agentJSON struct {
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools,omitempty"`
	Model       string   `json:"model"`
}

// BuildAgentsJSON renders the tier-3 agent definitions as the single JSON
// argument for --agents. Every agent's model is forced to the tier's
// VirtualModel (the primary tier-3 routing mechanism — see ARCHITECTURE.md),
// and an empty Prompt defaults to the Description. Output is deterministic:
// json.Marshal sorts map keys.
func BuildAgentsJSON(cfg *config.Config) (string, error) {
	sub := cfg.Tiers.Subagents
	out := make(map[string]agentJSON, len(sub.Agents))
	for name, a := range sub.Agents {
		prompt := a.Prompt
		if prompt == "" {
			prompt = a.Description
		}
		out[name] = agentJSON{
			Description: a.Description,
			Prompt:      prompt,
			Tools:       a.Tools,
			Model:       sub.VirtualModel,
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("marshal agents json: %w", err)
	}
	return string(b), nil
}
