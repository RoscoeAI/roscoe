package main

import (
	"fmt"
	"sort"
	"strings"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/models"
)

// settingRow is one line of the settings panel. A heading groups the rows
// under it. A row with a path can be edited; one without shows why not, so
// the surface stays honest about which knobs actually exist.
type settingRow struct {
	heading string
	label   string
	path    string
	// raw is the stored value, which is what an edit starts from; display is
	// what the panel shows and may read better ("8 at once" for 8). Held here
	// rather than fetched, because a setting left at its zero value is absent
	// from the marshalled config and could not be read back.
	raw     string
	display string
	why     string
	choices []string
}

func (r settingRow) shown() string {
	if r.display != "" {
		return r.display
	}
	return r.raw
}

func (r settingRow) editable() bool { return r.path != "" }

// settingsRows is the whole fleet on one surface: every tier's model,
// provider, and effort, in the order the work flows through them.
func settingsRows(cfg *config.Config) []settingRow {
	return settingsRowsWith(cfg, nil)
}

// settingsRowsWith renders the panel, resolving model aliases through the
// catalogue when one is loaded: "opus" alone cannot tell you which opus.
func settingsRowsWith(cfg *config.Config, cat *models.Catalog) []settingRow {
	show := func(provider, alias string) string {
		if cat == nil {
			return alias
		}
		return cat.Describe(provider, alias)
	}
	_ = show
	providers := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		providers = append(providers, name)
	}
	modelList := modelChoicesWith(cfg, cat)

	return []settingRow{
		{heading: "tier 1   your session, the one you talk to"},
		{label: "model", path: "tiers.main.model", raw: cfg.Tiers.Main.Model,
			display: show(cfg.Tiers.Main.Provider, cfg.Tiers.Main.Model), choices: modelList},
		{label: "provider", path: "tiers.main.provider", raw: cfg.Tiers.Main.Provider, choices: providers},
		{label: "effort", path: "tiers.main.effort",
			raw: cfg.Tiers.Main.Effort, display: effortDisplay(cfg.Tiers.Main.Effort),
			choices: config.EffortLevels()},

		{heading: "tier 2   workers, one spawned per task"},
		{label: "model", path: "tiers.middle.model", raw: cfg.Tiers.Middle.Model,
			display: middleModelDisplay(cfg, show), choices: modelList},
		{label: "provider", path: "tiers.middle.provider", raw: cfg.Tiers.Middle.Provider, choices: providers},
		{label: "effort", path: "tiers.middle.effort", raw: cfg.Tiers.Middle.Effort, display: effortDisplay(cfg.Tiers.Middle.Effort), choices: config.EffortLevels()},
		{label: "harness", path: "tiers.middle.harness", raw: harnessOf(cfg), choices: []string{"claude", "codex"}},
		{label: "lean", path: "tiers.middle.lean_context",
			raw:     boolStr(cfg.Tiers.Middle.Lean()),
			display: leanDisplay(cfg.Tiers.Middle.Lean()),
			choices: []string{"true", "false"}},
		{label: "mcp", why: mcpSummary(cfg)},
		{label: "cache", path: "tiers.middle.cache_ttl",
			raw: cfg.Tiers.Middle.TTL(), display: cfg.Tiers.Middle.TTL() + " prompt cache",
			choices: []string{"1h", "5m"}},

		{heading: "tier 3   the swarm each worker fans out to"},
		{label: "model", path: "tiers.subagents.model", raw: cfg.Tiers.Subagents.Model,
			display: show(cfg.Tiers.Subagents.Provider, cfg.Tiers.Subagents.Model), choices: modelList},
		{label: "provider", path: "tiers.subagents.provider", raw: cfg.Tiers.Subagents.Provider, choices: providers},
		{label: "effort", why: "claude code has no per-subagent effort knob; tier 2's applies"},
		{label: "width", path: "tiers.subagents.max_concurrent",
			raw:     fmt.Sprintf("%d", cfg.Tiers.Subagents.MaxConcurrent),
			display: fmt.Sprintf("%d at once", cfg.Tiers.Subagents.MaxConcurrent), choices: []string{"1", "2", "4", "8", "12", "16", "24"}},

		{heading: "fleet"},
		{label: "autonomy", path: "autonomy.level",
			raw: fmt.Sprintf("%d", cfg.Autonomy.Level), choices: []string{"0", "25", "50", "75", "90", "100"}},
	}
}

// mcpSummary names the servers a worker gets. Editing a map is beyond the
// arrow keys, so the row reads rather than edits and points at /config.
func mcpSummary(cfg *config.Config) string {
	srv := cfg.Tiers.Middle.MCPServers
	if len(srv) == 0 {
		return "none   set tiers.middle.mcp_servers to give workers a server"
	}
	names := make([]string, 0, len(srv))
	for n := range srv {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Sprintf("%d: %s   (edit with /config tiers.middle.mcp_servers)", len(names), strings.Join(names, ", "))
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// leanDisplay says what the setting does, not just its value: "true" alone
// tells you nothing about what it strips.
func leanDisplay(lean bool) string {
	if lean {
		return "true   no MCP servers or personal skills, much cheaper prefix"
	}
	return "false  workers load your full ~/.claude"
}

// effortDisplay names the empty case rather than showing a blank cell, so
// "unset" is never mistaken for "off".
func effortDisplay(e string) string {
	if e != "" {
		return e
	}
	return "claude's default"
}

// middleModelDisplay says what tier 2 will actually run. Under codex the
// configured value may be a claude leftover that codex never sees, in which
// case the row names codex's own model rather than lying with "sonnet".
func middleModelDisplay(cfg *config.Config, show func(provider, alias string) string) string {
	if harnessOf(cfg) == "codex" {
		m, passed := models.CodexModel(cfg.Tiers.Middle.Model)
		if passed {
			return m
		}
		return m + "   (codex's own; tiers.middle.model is a claude name)"
	}
	return show(cfg.Tiers.Middle.Provider, cfg.Tiers.Middle.Model)
}

func harnessOf(cfg *config.Config) string {
	if h := cfg.Tiers.Middle.Harness; h != "" {
		return h
	}
	return "claude"
}

// renderSettings paints the panel, windowed so the selected row stays on
// screen on a short terminal.
func renderSettings(rows []settingRow, sel, height int) []string {
	width := 0
	for _, r := range rows {
		if n := len(r.label); n > width {
			width = n
		}
	}

	out := make([]string, 0, len(rows)+6)
	lineOf := make([]int, len(rows))
	for i, r := range rows {
		if r.heading != "" {
			if i > 0 {
				out = append(out, "")
			}
			lineOf[i] = len(out)
			out = append(out, "  "+ansiFaint+r.heading+ansiReset)
			continue
		}
		lineOf[i] = len(out)
		marker := "    "
		label := ansiDim + r.label + ansiReset
		if i == sel {
			marker = "  " + ansiGreen + "› " + ansiReset
			label = ansiGreen + r.label + ansiReset
		}
		value := r.shown()
		if !r.editable() {
			value = ansiFaint + r.why + ansiReset
		}
		pad := width - len(r.label)
		if pad < 0 {
			pad = 0
		}
		out = append(out, marker+label+strings.Repeat(" ", pad)+"   "+value)
	}

	// Keep the selection in view rather than clipping the tail.
	if len(out) > height {
		start := lineOf[sel] - height/2
		if start < 0 {
			start = 0
		}
		if start+height > len(out) {
			start = len(out) - height
		}
		out = out[start : start+height]
	}
	return out
}

// nextSelectable walks from sel in direction step to the next editable row,
// stopping at the ends rather than wrapping.
func nextSelectable(rows []settingRow, sel, step int) int {
	for i := sel + step; i >= 0 && i < len(rows); i += step {
		if rows[i].editable() {
			return i
		}
	}
	return sel
}

func firstSelectable(rows []settingRow) int {
	for i, r := range rows {
		if r.editable() {
			return i
		}
	}
	return 0
}

// runSettings takes over the viewport with the settings panel: up and down
// move, enter edits the selected value, esc closes. Every edit is validated
// and persisted before the panel redraws.
func runSettings(sc *screen, keys *keyReader, cfg *config.Config, explicit string) {
	// Resolve aliases so the panel can say which opus.
	cat := models.Open(cfg.StateDir)
	rows := settingsRowsWith(cfg, cat)
	sel := firstSelectable(rows)
	defer sc.Overlay(nil)

	for {
		sc.Overlay(renderSettings(rows, sel, sc.ViewHeight()))
		sc.SetPrompt("", "", "  ↑↓ move · ←→ change · enter type · esc close", noteForRow(rows[sel]))

		key := keys.NextKey()
		switch key {
		case "up":
			sel = nextSelectable(rows, sel, -1)
		case "down":
			sel = nextSelectable(rows, sel, 1)
		case "left", "right":
			// Most of these are short lists, so stepping through them beats
			// retyping a value.
			row := rows[sel]
			step := 1
			if key == "left" {
				step = -1
			}
			if next := cycleChoice(row.choices, row.raw, step); next != "" {
				applySetting(sc, cfg, explicit, row, next)
				rows = settingsRowsWith(cfg, cat)
			}
		case "enter":
			row := rows[sel]
			comp := &completer{candidates: func(input string) []string {
				return matching(row.choices, strings.TrimSpace(input))
			}}
			val, ok := keys.ReadLineOn(sc, row.label+" = ", row.raw, nil, comp)
			if ok {
				applySetting(sc, cfg, explicit, row, strings.TrimSpace(val))
			}
			rows = settingsRowsWith(cfg, cat)
		case "esc", "ctrl-c", "eof":
			sc.SetPrompt("", "", "", "")
			return
		}
	}
}

func noteForRow(row settingRow) string {
	if !row.editable() {
		return row.label + ": " + row.why
	}
	if d := config.Describe(row.path); d != "" {
		return row.path + ": " + d
	}
	return row.path
}

// applySetting validates in memory before writing, and puts the old value
// back if the new one would make the config invalid.
func applySetting(sc *screen, cfg *config.Config, explicit string, row settingRow, val string) {
	if val == "" {
		return
	}
	old := row.raw
	if err := cfg.SetPath(row.path, val); err != nil {
		sc.Printf("%s%v%s", ansiDim, err, ansiReset)
		return
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		_ = cfg.SetPath(row.path, old)
		sc.Printf("%s%v%s", ansiDim, errs[0], ansiReset)
		return
	}
	persist(sc, explicit, row.path, val)
}

// cycleChoice returns the choice step positions from current, clamped at the
// ends. An empty result means there is nothing to cycle.
func cycleChoice(choices []string, current string, step int) string {
	if len(choices) == 0 {
		return ""
	}
	at := -1
	for i, c := range choices {
		if c == current {
			at = i
			break
		}
	}
	if at < 0 { // current value is not in the list; enter it at either end
		if step > 0 {
			return choices[0]
		}
		return choices[len(choices)-1]
	}
	next := at + step
	if next < 0 || next >= len(choices) {
		return ""
	}
	return choices[next]
}
