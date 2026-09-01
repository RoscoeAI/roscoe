package loop

import (
	"strings"
)

// Tail is the structured block a worker appends to its final message instead
// of editing loop.md itself. The worker stays the author, which is what keeps
// fidelity: it writes while it still holds the whole turn, tool results and
// dead ends included. What changes is only the destination, so it pays no tool
// calls and the format is parsed rather than trusted.
type Tail struct {
	Status string
	Plan   string
	Tried  string
	Notes  string
}

// Empty reports a tail that carries nothing worth applying.
func (t Tail) Empty() bool {
	return strings.TrimSpace(t.Status) == "" && strings.TrimSpace(t.Plan) == "" &&
		strings.TrimSpace(t.Tried) == "" && strings.TrimSpace(t.Notes) == ""
}

// tailFence opens the block. Closing is any ``` line.
const tailFence = "```loop"

// ParseTail finds the loop block in a worker's final message. Models fence and
// preface output loosely, so this looks for the last such block: a worker that
// shows the format before filling it in should not have its example parsed.
func ParseTail(result string) (Tail, bool) {
	lower := strings.ToLower(result)
	start := strings.LastIndex(lower, tailFence)
	if start < 0 {
		return Tail{}, false
	}
	rest := result[start+len(tailFence):]
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	} else {
		return Tail{}, false
	}
	if end := strings.Index(rest, "```"); end >= 0 {
		rest = rest[:end]
	}
	t := parseTailBody(rest)
	return t, !t.Empty()
}

// parseTailBody reads KEY: lines, where a key's value is either the remainder
// of its own line or the lines up to the next key.
func parseTailBody(body string) Tail {
	var t Tail
	field := ""
	var buf []string
	flush := func() {
		v := strings.Trim(strings.Join(buf, "\n"), "\n")
		buf = nil
		switch field {
		case "status":
			t.Status = strings.TrimSpace(v)
		case "plan":
			t.Plan = v
		case "tried":
			t.Tried = v
		case "notes":
			t.Notes = v
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if key, val, ok := tailKey(line); ok {
			flush()
			field = key
			if strings.TrimSpace(val) != "" {
				buf = append(buf, strings.TrimSpace(val))
			}
			continue
		}
		if field != "" {
			buf = append(buf, line)
		}
	}
	flush()
	return t
}

// tailKey recognises a leading "STATUS:" style key, tolerating markdown
// emphasis and list markers around it.
func tailKey(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	t = strings.TrimLeft(t, "-*# ")
	i := strings.IndexByte(t, ':')
	if i <= 0 {
		return "", "", false
	}
	name := strings.ToLower(strings.Trim(t[:i], " *_`"))
	switch name {
	case "status", "plan", "tried", "notes":
		// A key written as **STATUS:** leaves its closing emphasis on the
		// value; a value that keeps it fails every later comparison.
		return name, strings.TrimLeft(t[i+1:], " *_`"), true
	}
	return "", "", false
}

// ApplyTail folds a tail into loop.md. Status and Plan are replaced because
// they describe now; Tried and Notes are appended because they are the record.
// Entries already present are not appended twice, so a worker that both writes
// the file and emits a tail does not double up.
func ApplyTail(md string, t Tail) string {
	out := md
	if s := strings.TrimSpace(t.Status); s != "" {
		out = setSection(out, "Status", normalizeStatusLine(s))
	}
	if p := strings.TrimSpace(t.Plan); p != "" {
		out = setSection(out, "Plan", p)
	}
	out = appendEntries(out, "Tried", t.Tried)
	out = appendEntries(out, "Notes", t.Notes)
	return out
}

// normalizeStatusLine keeps only the recognised word, so "STATUS: continuing
// (still work to do)" does not defeat ParseStatus.
func normalizeStatusLine(s string) string {
	switch ParseStatus("## Status\n" + s + "\n") {
	case StatusDone:
		return StatusDone
	case StatusBlocked:
		return StatusBlocked
	default:
		for _, w := range []string{StatusDone, StatusBlocked, StatusContinuing} {
			if strings.Contains(strings.ToLower(s), w) {
				return w
			}
		}
		return StatusContinuing
	}
}

func appendEntries(md, section, body string) string {
	adds := splitEntries(body)
	if len(adds) == 0 {
		return md
	}
	cur := Section(md, section)
	existing := splitEntries(cur)
	var fresh []string
	for _, a := range adds {
		if !covered(a, existing) {
			fresh = append(fresh, a)
			existing = append(existing, a)
		}
	}
	if len(fresh) == 0 {
		return md
	}
	merged := strings.TrimRight(cur, "\n")
	// The seed's filler is not content to append to.
	if len(splitEntries(cur)) == 0 {
		merged = ""
	}
	if merged != "" {
		merged += "\n"
	}
	return setSection(md, section, merged+strings.Join(fresh, "\n"))
}

// DefaultProjectedTried is how many recent Tried entries ride along in the
// prompt. The rest stay in the file, which the worker can still read.
const DefaultProjectedTried = 5

// Projection renders the part of loop.md worth putting in every prompt.
// Inlining the whole file would re-inject it into the transcript every
// iteration, which grows quadratically on a path where nothing trims it, so
// Status, Plan and Notes go in full and Tried is capped at its most recent
// entries.
func Projection(md string, maxTried int) string {
	if strings.TrimSpace(md) == "" {
		return ""
	}
	if maxTried <= 0 {
		maxTried = DefaultProjectedTried
	}
	var b strings.Builder
	write := func(name, body string) {
		if strings.TrimSpace(body) == "" {
			return
		}
		b.WriteString("## " + name + "\n" + strings.TrimSpace(body) + "\n\n")
	}
	write("Status", Section(md, "Status"))
	write(RecalledSection, Section(md, RecalledSection))
	write("Plan", Section(md, "Plan"))

	tried := splitEntries(Section(md, "Tried"))
	if n := len(tried); n > 0 {
		shown := tried
		omitted := 0
		if n > maxTried {
			shown = tried[n-maxTried:]
			omitted = n - maxTried
		}
		body := strings.Join(shown, "\n")
		if omitted > 0 {
			body = "(" + itoa(omitted) + " earlier entries are in loop.md)\n" + body
		}
		write("Tried", body)
	}
	write("Notes", Section(md, "Notes"))
	return strings.TrimRight(b.String(), "\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}
