package main

import (
	"fmt"
	"strings"

	"roscoe.sh/roscoe/internal/config"
)

// configRow is one line of a settings listing: a branch you can open, or a
// leaf with its value. The same rows feed the chat screen and the CLI.
type configRow struct {
	Name   string // the last path segment, what the person reads
	Path   string // the full dotted path, what they type
	Value  string // "" for a branch
	What   string
	Branch bool
	Rare   bool // one-time setup, shown apart at the top level
}

// levelRows lists what is under prefix, most-changed first.
func levelRows(cfg *config.Config, prefix string) []configRow {
	kids := config.Ordered(prefix, cfg.ChildPaths(prefix))
	rows := make([]configRow, 0, len(kids))
	for _, k := range kids {
		r := configRow{Name: strings.TrimPrefix(k, prefix+"."), Path: k, What: config.Describe(k), Rare: prefix == "" && config.Rare(k)}
		if len(cfg.ChildPaths(k)) > 0 {
			r.Branch = true
		} else {
			r.Value = valueOf(cfg, k)
		}
		rows = append(rows, r)
	}
	return rows
}

// leafCard is everything to know before changing one setting: what it is set
// to, what it does, what it can be, what the choice means, and how to change
// it. setHint is the command form of this surface ("/config" or
// "roscoe config set"), and shortcut the one-word command when there is one.
type leafCard struct {
	Path, Value string
	What        string
	Options     string // "low · medium · high" or a format
	Means       string
	SetIt       string
}

// valueOf is what a leaf is set to, as a person should read it: the file's
// value, else the implied default marked as such, else "(unset)".
func valueOf(cfg *config.Config, path string) string {
	if v, err := cfg.Get(path); err == nil {
		s := fmt.Sprintf("%v", v)
		if s != "" && s != "[]" && s != "map[]" {
			return s
		}
	}
	if def, ok := config.Implied(path); ok {
		return def + " (default)"
	}
	return "(unset)"
}

func buildLeafCard(cfg *config.Config, path, setHint, shortcut string) leafCard {
	d := config.DocFor(path)
	c := leafCard{Path: path, What: d.What, Means: d.Effect, Value: valueOf(cfg, path)}
	switch {
	case len(d.Choices) > 0:
		c.Options = strings.Join(d.Choices, " · ")
	case d.Format != "":
		c.Options = d.Format
	}
	c.SetIt = setHint + " " + path + " <value>"
	if shortcut != "" {
		c.SetIt += "   or   " + shortcut + " <value>"
	}
	return c
}

// lines renders the card plainly, one labelled line each, for the CLI and for
// tests. The chat screen colours the same lines.
func (c leafCard) lines() []string {
	out := []string{c.Path + " = " + c.Value}
	if c.What != "" {
		out = append(out, "  "+c.What)
	}
	if c.Options != "" {
		out = append(out, "  options  "+c.Options)
	}
	if c.Means != "" {
		out = append(out, "  means    "+c.Means)
	}
	out = append(out, "  set it   "+c.SetIt)
	return out
}

// shortcuts maps the one-word chat commands to the setting they change, so a
// card can say both spellings and the two can never disagree. Only fleet-wide
// settings get one: a shortcut for a tier's knob hides which tier.
var shortcuts = map[string]string{
	"/autonomy": "autonomy.level",
}

// shortcutFor is the one-word command for a path, or "".
func shortcutFor(path string) string {
	for cmd, p := range shortcuts {
		if p == path {
			return cmd
		}
	}
	return ""
}

// levelLines renders a listing plainly: branches by what they hold, leaves
// with their value, setup keys under their own heading at the top level.
func levelLines(cfg *config.Config, prefix string) []string {
	rows := levelRows(cfg, prefix)
	if len(rows) == 0 {
		return []string{"no settings under " + prefix}
	}
	width, vwidth := 0, 0
	for _, r := range rows {
		if len(r.Name) > width {
			width = len(r.Name)
		}
		if n := len(r.Value); n > vwidth && n <= 24 {
			vwidth = n
		}
	}
	var out []string
	if d := config.Describe(prefix); d != "" {
		out = append(out, d)
	}
	rareShown := false
	for _, r := range rows {
		if r.Rare && !rareShown {
			out = append(out, "", "set up once")
			rareShown = true
		}
		switch {
		case r.Branch:
			out = append(out, fmt.Sprintf("  %-*s  %s", width, r.Name, r.What))
		default:
			out = append(out, fmt.Sprintf("  %-*s  %-*s  %s", width, r.Name, vwidth, r.Value, r.What))
		}
	}
	return out
}
