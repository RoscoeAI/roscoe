package loop

import (
	"strings"
)

// preservedSections are the parts of loop.md that accumulate. Status and Plan
// are meant to be replaced each iteration; Tried and Notes are the record, and
// losing an entry there is how a loop forgets a dead end and walks back into
// it.
var preservedSections = []string{"Tried", "Notes"}

// MergePreserving returns after, with any Tried or Notes entry that existed in
// before and vanished from after put back. It also reports what it restored.
//
// The worker rewrites the whole file every iteration, which means one careless
// rewrite can drop what every earlier iteration learned. Rather than police the
// worker with a stricter prompt, which is a contract a model can always break,
// the supervisor makes the loss impossible: the file is whatever the worker
// wrote, plus anything it forgot.
func MergePreserving(before, after string) (string, []string) {
	if strings.TrimSpace(before) == "" {
		return after, nil
	}
	if strings.TrimSpace(after) == "" {
		// The worker truncated the file entirely. Its own memory is worth more
		// than an empty file, so keep what was there.
		return before, []string{"the whole file"}
	}

	out := after
	var restored []string
	for _, name := range preservedSections {
		olds := splitEntries(Section(before, name))
		if len(olds) == 0 {
			continue
		}
		body := Section(out, name)
		news := splitEntries(body)

		var missing []string
		for _, e := range olds {
			if !covered(e, news) {
				missing = append(missing, e)
			}
		}
		if len(missing) == 0 {
			continue
		}
		restored = append(restored, missing...)
		merged := strings.TrimRight(body, "\n")
		if merged != "" {
			merged += "\n"
		}
		merged += strings.Join(missing, "\n")
		out = setSection(out, name, merged)
	}
	return out, restored
}

// covered reports whether an earlier entry survives among the new ones. An
// entry that was expanded still counts as kept, so editing a line does not
// resurrect its older self; an entry that was shortened does not, because the
// safe direction is to keep the longer text.
func covered(old string, news []string) bool {
	o := normalizeEntry(old)
	if o == "" {
		return true
	}
	for _, n := range news {
		if strings.Contains(normalizeEntry(n), o) {
			return true
		}
	}
	return false
}

func normalizeEntry(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("*", "", "_", "", "`", "", "[ ]", "", "[x]", "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// splitEntries breaks a section body into the units worth preserving: one per
// top-level list item including its indented continuation lines, or one per
// paragraph where the section is prose. Placeholder text from the seed is
// dropped, so an untouched "_nothing yet_" is never restored over real content.
func splitEntries(body string) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	var out []string
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		e := strings.TrimRight(strings.Join(cur, "\n"), "\n")
		cur = nil
		if t := strings.TrimSpace(e); t != "" && !isPlaceholder(t) {
			out = append(out, e)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		switch {
		case isListItem(line):
			flush()
			cur = append(cur, line)
		case strings.TrimSpace(line) == "":
			// A blank line ends a paragraph but not a list item with an
			// indented continuation, which is rare enough to treat simply.
			flush()
		default:
			cur = append(cur, line)
		}
	}
	flush()
	return out
}

func isListItem(line string) bool {
	t := strings.TrimLeft(line, " \t")
	if len(t) == len(line) { // not indented: a genuine top-level item
		return strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")
	}
	return false
}

// isPlaceholder recognises the seed's own filler so it is never preserved over
// what a worker actually wrote.
func isPlaceholder(s string) bool {
	t := strings.ToLower(strings.Trim(s, "_* "))
	return t == "nothing yet" ||
		strings.HasPrefix(t, "durable facts about this codebase go here")
}

// setSection replaces a section's body, appending the section when absent.
func setSection(md, name, body string) string {
	lines := strings.Split(md, "\n")
	start := -1
	for i, line := range lines {
		if isHeading(line, name) {
			start = i
			break
		}
	}
	if start < 0 {
		out := strings.TrimRight(md, "\n")
		return out + "\n\n## " + name + "\n" + body + "\n"
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			end = i
			break
		}
	}
	rebuilt := append([]string{}, lines[:start+1]...)
	rebuilt = append(rebuilt, strings.Split(body, "\n")...)
	if end < len(lines) {
		rebuilt = append(rebuilt, "")
	}
	rebuilt = append(rebuilt, lines[end:]...)
	return strings.Join(rebuilt, "\n")
}
