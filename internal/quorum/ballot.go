package quorum

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"roscoe.sh/roscoe/internal/config"
	"roscoe.sh/roscoe/internal/loop"
)

// maxBallotBytes caps what a voter is shown. A voter reads the working memory
// and the result, not the transcript, so this is generous; the cap only stops
// a runaway loop.md from turning every vote into a large request.
const maxBallotBytes = 24 << 10

// ballotPrompt is what each voter is asked. It states the decision as the
// voter's own, names the categories it must flag, and demands one JSON object,
// because a vote that cannot be parsed is a vote that did not happen.
func (q *Quorum) ballotPrompt(it loop.Iteration) string {
	var b strings.Builder
	b.WriteString(`You are one voter deciding whether an autonomous coding loop should keep going.

You are judging the work, not the worker's opinion of it. The worker wrote its own status line; treat that as a claim to check, not as the answer.

Decide one of:
- "done"      the charter is genuinely and fully satisfied
- "continue"  there is more useful work to do toward the charter
- "escalate"  a human decision is needed that no model should make alone

Also list any of these categories the iteration touched, as "kinds":
`)
	for _, k := range q.AlwaysEscalate {
		fmt.Fprintf(&b, "  - %s\n", k)
	}
	if len(q.AlwaysEscalate) == 0 {
		b.WriteString("  (none configured)\n")
	}
	b.WriteString(`
Reply with ONE JSON object and nothing else:
{"action":"done|continue|escalate","confidence":0.0-1.0,"reason":"one sentence","kinds":[]}

--- CHARTER ---
`)
	b.WriteString(strings.TrimSpace(it.Charter))
	b.WriteString("\n\n--- WORKING MEMORY (loop.md, as the worker left it) ---\n")
	b.WriteString(clip(it.LoopMD, maxBallotBytes))
	b.WriteString("\n\n--- WHAT THE WORKER SAID IT DID THIS ITERATION ---\n")
	if it.Result != nil {
		b.WriteString(clip(it.Result.Result, 8<<10))
	} else {
		b.WriteString("(no result)")
	}
	fmt.Fprintf(&b, "\n\n--- ITERATION %d, $%.4f spent so far ---\n", it.N, it.SpentUSD)
	return b.String()
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// Keep the tail: the most recent entries in Tried and Notes matter more
	// than the charter header, which the voter is given separately anyway.
	return "…(truncated)…\n" + s[len(s)-max:]
}

// ask puts the ballot to one voter and parses its answer.
func (q *Quorum) ask(ctx context.Context, v config.Voter, it loop.Iteration) Verdict {
	name := v.Provider + "/" + v.Model
	prov, ok := q.Providers[v.Provider]
	if !ok {
		return Verdict{Voter: name, Err: fmt.Errorf("unknown provider %q", v.Provider)}
	}
	auth, err := q.resolveAuth(prov, v.Account)
	if err != nil {
		return Verdict{Voter: name, Err: fmt.Errorf("auth: %w", err)}
	}

	body, err := json.Marshal(map[string]any{
		"model":      v.Model,
		"max_tokens": 512,
		"messages": []map[string]string{
			{"role": "user", "content": q.ballotPrompt(it)},
		},
	})
	if err != nil {
		return Verdict{Voter: name, Err: fmt.Errorf("marshal ballot: %w", err)}
	}

	url := strings.TrimRight(prov.BaseURL, "/") + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Verdict{Voter: name, Err: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := q.client().Do(req)
	if err != nil {
		return Verdict{Voter: name, Err: fmt.Errorf("request: %w", err)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		return Verdict{Voter: name, Err: fmt.Errorf("http %d: %s", resp.StatusCode, oneLine(string(raw), 200))}
	}

	text, err := anthropicText(raw)
	if err != nil {
		return Verdict{Voter: name, Err: err}
	}
	ver, err := parseVerdict(text)
	if err != nil {
		return Verdict{Voter: name, Err: err}
	}
	ver.Voter = name
	return ver
}

// anthropicText pulls the assistant text out of a Messages response.
func anthropicText(raw []byte) (string, error) {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("no text in response")
	}
	return b.String(), nil
}

// parseVerdict reads the voter's JSON. Models wrap JSON in prose and fences
// often enough that finding the object is part of the job, not a workaround:
// a vote thrown away for punctuation is a voter silently removed from the
// quorum.
func parseVerdict(text string) (Verdict, error) {
	obj, ok := firstJSONObject(text)
	if !ok {
		return Verdict{}, fmt.Errorf("no JSON object in %q", oneLine(text, 120))
	}
	var p struct {
		Action     string   `json:"action"`
		Confidence float64  `json:"confidence"`
		Reason     string   `json:"reason"`
		Kinds      []string `json:"kinds"`
	}
	if err := json.Unmarshal([]byte(obj), &p); err != nil {
		return Verdict{}, fmt.Errorf("decode verdict: %w", err)
	}
	act, err := parseAction(p.Action)
	if err != nil {
		return Verdict{}, err
	}
	// An absent confidence reads as certain, which would let a voter that
	// omitted the field drag the mean below the floor. Treat it as full.
	conf := p.Confidence
	if conf <= 0 {
		conf = 1
	}
	if conf > 1 {
		conf = 1
	}
	return Verdict{Action: act, Confidence: conf, Reason: p.Reason, Kinds: p.Kinds}, nil
}

func parseAction(s string) (loop.Action, error) {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(s), `"'.`)) {
	case "done":
		return loop.Done, nil
	case "continue":
		return loop.Continue, nil
	case "escalate":
		return loop.Escalate, nil
	case "abort":
		return loop.Abort, nil
	default:
		return "", fmt.Errorf("unrecognised action %q", s)
	}
}

// firstJSONObject returns the first balanced {...} run, ignoring braces inside
// strings so a reason containing "{" does not truncate the object.
func firstJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// nothing
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}
