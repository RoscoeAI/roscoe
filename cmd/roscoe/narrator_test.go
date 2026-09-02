package main

import (
	"encoding/json"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/streamjson"
)

func evt(raw string) *streamjson.Event {
	return &streamjson.Event{Type: typeOfRaw(raw), Subtype: subtypeOfRaw(raw), Raw: []byte(raw)}
}

func typeOfRaw(raw string) string {
	var r struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(raw), &r)
	return r.Type
}

func subtypeOfRaw(raw string) string {
	if strings.Contains(raw, `"subtype":"init"`) {
		return "init"
	}
	return ""
}

func delta(text string) *streamjson.Event {
	return evt(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"` + text + `"}},"parent_tool_use_id":null}`)
}

func subDelta(text string) *streamjson.Event {
	return evt(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"` + text + `"}},"parent_tool_use_id":"toolu_1"}`)
}

var blockStop = evt(`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`)

// The sequence a real turn produces: deltas, block stop, then the whole
// assistant message repeating the same text. The text must appear once.
func TestNarratorStreamsWithoutDoublePrinting(t *testing.T) {
	sc := bufScreen()
	n := &narrator{sc: sc}
	for _, e := range []*streamjson.Event{delta("al"), delta("p"), delta("ha beta gamma"), blockStop} {
		n.event(e)
	}
	n.event(evt(`{"type":"assistant","message":{"content":[{"type":"text","text":"alpha beta gamma"}]}}`))

	joined := strings.Join(sc.lines, "\n")
	if c := strings.Count(joined, "alpha beta gamma"); c != 1 {
		t.Errorf("text appears %d times, want once:\n%s", c, joined)
	}
	if !n.StreamedAny() {
		t.Error("streamedAny not set")
	}
	if sc.Streaming() {
		t.Error("stream not ended by block stop")
	}
}

// A message whose text was streamed but which also called tools still gets
// its tool line; only the text is suppressed.
func TestNarratorStillPrintsToolsAfterStreaming(t *testing.T) {
	sc := bufScreen()
	n := &narrator{sc: sc}
	n.event(delta("Let me check."))
	n.event(blockStop)
	n.event(evt(`{"type":"assistant","message":{"content":[{"type":"text","text":"Let me check."},{"type":"tool_use","name":"Read"}]}}`))
	joined := strings.Join(sc.lines, "\n")
	if strings.Count(joined, "Let me check.") != 1 {
		t.Errorf("text duplicated:\n%s", joined)
	}
	if !strings.Contains(joined, "Read") {
		t.Errorf("tool line missing:\n%s", joined)
	}
}

// Without partial messages there are no deltas, and the whole-message path
// must still print the text (the older behaviour).
func TestNarratorPrintsTextWhenNothingStreamed(t *testing.T) {
	sc := bufScreen()
	n := &narrator{sc: sc}
	n.event(evt(`{"type":"assistant","message":{"content":[{"type":"text","text":"whole answer"}]}}`))
	if !strings.Contains(strings.Join(sc.lines, "\n"), "whole answer") {
		t.Errorf("unstreamed text not printed: %q", sc.lines)
	}
	if n.StreamedAny() {
		t.Error("streamedAny set with no deltas")
	}
}

// Subagent text is forwarded with a parent id and must not stream into the
// main line, or every parallel worker writes over one another.
func TestNarratorIgnoresSubagentDeltas(t *testing.T) {
	sc := bufScreen()
	n := &narrator{sc: sc}
	n.event(subDelta("worker chatter"))
	n.event(subDelta("more"))
	if len(sc.lines) != 0 || n.StreamedAny() {
		t.Errorf("subagent deltas were rendered: %q streamedAny=%v", sc.lines, n.StreamedAny())
	}
}

// State is per message: text streamed for message one must not suppress the
// printing of message two if that one arrives whole.
func TestNarratorResetsPerMessage(t *testing.T) {
	sc := bufScreen()
	n := &narrator{sc: sc}
	n.event(delta("first"))
	n.event(blockStop)
	n.event(evt(`{"type":"assistant","message":{"content":[{"type":"text","text":"first"}]}}`))
	n.event(evt(`{"type":"assistant","message":{"content":[{"type":"text","text":"second, unstreamed"}]}}`))
	joined := strings.Join(sc.lines, "\n")
	if strings.Count(joined, "first") != 1 || !strings.Contains(joined, "second, unstreamed") {
		t.Errorf("lines = %q", sc.lines)
	}
}

// On the plain stderr path a whole line arriving mid-stream (a rate-limit
// notice, a tool call) must start on a fresh line, not glue itself onto the
// half-written sentence; and the stream must end with exactly one newline.
func TestNarratorStderrLineDiscipline(t *testing.T) {
	var buf strings.Builder
	n := &narrator{errW: &buf}
	n.event(delta("alpha "))
	n.event(delta("beta"))
	n.event(evt(`{"type":"rate_limit_event"}`))
	n.event(delta(" gamma"))
	n.event(blockStop)
	n.event(evt(`{"type":"assistant","message":{"content":[{"type":"text","text":"alpha beta gamma"}]}}`))
	got := buf.String()
	want := "alpha beta\n[rate_limit_event]\n gamma\n"
	if got != want {
		t.Errorf("stderr =\n%q\nwant\n%q", got, want)
	}
	if strings.Count(got, "alpha beta") != 1 {
		t.Error("streamed text was printed again by the whole-message event")
	}
}
