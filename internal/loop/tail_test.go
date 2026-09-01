package loop

import (
	"context"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/streamjson"
)

const goodTail = "Done for now.\n\n```loop\n" +
	"STATUS: continuing\n" +
	"PLAN:\n- [x] first step\n- [ ] second step\n" +
	"TRIED:\n- the fast path deadlocked under load\n" +
	"NOTES:\n- the build needs CGO_ENABLED=0\n" +
	"```\n"

func TestParseTail(t *testing.T) {
	tail, ok := ParseTail(goodTail)
	if !ok {
		t.Fatal("a well-formed block did not parse")
	}
	if tail.Status != "continuing" {
		t.Errorf("status = %q", tail.Status)
	}
	if !strings.Contains(tail.Plan, "- [x] first step") || !strings.Contains(tail.Plan, "- [ ] second step") {
		t.Errorf("plan = %q", tail.Plan)
	}
	if !strings.Contains(tail.Tried, "deadlocked under load") {
		t.Errorf("tried = %q", tail.Tried)
	}
	if !strings.Contains(tail.Notes, "CGO_ENABLED=0") {
		t.Errorf("notes = %q", tail.Notes)
	}
}

func TestParseTailRejectsWhatIsNotThere(t *testing.T) {
	for name, in := range map[string]string{
		"no block":     "I did the work and it went fine.",
		"empty":        "",
		"fence only":   "```loop\n```",
		"unterminated": "```loop",
	} {
		if _, ok := ParseTail(in); ok {
			t.Errorf("%s: parsed a tail that is not there", name)
		}
	}
}

// A worker that shows the template before filling it in must not have its
// example parsed instead of its answer.
func TestParseTailTakesTheLastBlock(t *testing.T) {
	in := "Here is the format I will use:\n\n```loop\nSTATUS: continuing\n```\n\nAnd here is my answer:\n\n" +
		"```loop\nSTATUS: done\nTRIED:\n- the real work\n```\n"
	tail, ok := ParseTail(in)
	if !ok {
		t.Fatal("did not parse")
	}
	if tail.Status != "done" {
		t.Errorf("status = %q, want the last block's", tail.Status)
	}
	if !strings.Contains(tail.Tried, "the real work") {
		t.Errorf("tried = %q", tail.Tried)
	}
}

// Models decorate. A tail lost to a bold marker is a worker silently demoted
// to writing nothing.
func TestParseTailToleratesDecoration(t *testing.T) {
	in := "```loop\n**STATUS:** done\n- TRIED:\n  - it worked\n  NOTES:\n- a fact\n```"
	tail, ok := ParseTail(in)
	if !ok {
		t.Fatal("did not parse a decorated block")
	}
	if tail.Status != "done" {
		t.Errorf("status = %q", tail.Status)
	}
	if !strings.Contains(tail.Tried, "it worked") {
		t.Errorf("tried = %q", tail.Tried)
	}
	if !strings.Contains(tail.Notes, "a fact") {
		t.Errorf("notes = %q", tail.Notes)
	}
}

func TestApplyTailReplacesNowAndAppendsRecord(t *testing.T) {
	md := "# c\n\n## Status\ncontinuing\n\n## Plan\n- [ ] first step\n- [ ] second step\n\n" +
		"## Tried\n- iteration 1: something earlier\n\n## Notes\n- an older fact\n"
	tail, _ := ParseTail(goodTail)
	out := ApplyTail(md, tail)

	if !strings.Contains(Section(out, "Plan"), "- [x] first step") {
		t.Errorf("Plan was not replaced:\n%s", Section(out, "Plan"))
	}
	tried := Section(out, "Tried")
	if !strings.Contains(tried, "iteration 1: something earlier") {
		t.Error("Tried lost the earlier entry; it must append, not replace")
	}
	if !strings.Contains(tried, "deadlocked under load") {
		t.Error("Tried did not gain the new entry")
	}
	notes := Section(out, "Notes")
	if !strings.Contains(notes, "an older fact") || !strings.Contains(notes, "CGO_ENABLED=0") {
		t.Errorf("Notes should hold both:\n%s", notes)
	}
	if ParseStatus(out) != StatusContinuing {
		t.Errorf("status = %q", ParseStatus(out))
	}
}

// A worker in transition may both edit the file and emit a tail. Applying the
// tail must not duplicate what is already there.
func TestApplyTailIsIdempotent(t *testing.T) {
	md := Seed("c")
	tail, _ := ParseTail(goodTail)
	once := ApplyTail(md, tail)
	twice := ApplyTail(once, tail)
	if once != twice {
		t.Errorf("applying the same tail twice changed the file:\n%s", twice)
	}
	if n := strings.Count(twice, "deadlocked under load"); n != 1 {
		t.Errorf("entry appears %d times, want 1", n)
	}
}

// The seed's filler must not survive next to real content.
func TestApplyTailClearsSeedPlaceholders(t *testing.T) {
	tail, _ := ParseTail(goodTail)
	out := ApplyTail(Seed("c"), tail)
	if strings.Contains(out, "nothing yet") {
		t.Errorf("placeholder survived:\n%s", out)
	}
}

// A status line with commentary must still parse; the loop reads this to
// decide whether the run is over.
func TestApplyTailNormalizesStatus(t *testing.T) {
	for in, want := range map[string]string{
		"done":                       StatusDone,
		"DONE - charter satisfied":   StatusDone,
		"blocked (need a decision)":  StatusBlocked,
		"continuing, more work left": StatusContinuing,
		"finished-ish":               StatusContinuing,
	} {
		out := ApplyTail(Seed("c"), Tail{Status: in})
		if got := ParseStatus(out); got != want {
			t.Errorf("STATUS %q -> %q, want %q", in, got, want)
		}
	}
}

func TestApplyEmptyTailChangesNothing(t *testing.T) {
	md := Seed("c")
	if out := ApplyTail(md, Tail{}); out != md {
		t.Errorf("an empty tail rewrote the file:\n%s", out)
	}
}

// Inlining the whole file would re-inject a growing document every iteration,
// which is quadratic on a path where nothing trims the transcript.
func TestProjectionCapsTried(t *testing.T) {
	var b strings.Builder
	b.WriteString("## Status\ncontinuing\n\n## Plan\n- [ ] a\n\n## Tried\n")
	for i := 1; i <= 20; i++ {
		b.WriteString("- entry number " + itoa(i) + "\n")
	}
	b.WriteString("\n## Notes\n- a durable fact\n")
	md := b.String()

	p := Projection(md, 5)
	if strings.Contains(p, "entry number 1\n") && !strings.Contains(p, "entry number 16") {
		t.Error("kept the oldest entries instead of the newest")
	}
	for i := 16; i <= 20; i++ {
		if !strings.Contains(p, "entry number "+itoa(i)) {
			t.Errorf("projection dropped recent entry %d", i)
		}
	}
	if strings.Contains(p, "entry number 2\n") {
		t.Error("projection kept an entry beyond the cap")
	}
	if !strings.Contains(p, "15 earlier entries are in loop.md") {
		t.Errorf("projection should say what it omitted:\n%s", p)
	}
	// Status, Plan and Notes are small and always worth carrying.
	for _, want := range []string{"continuing", "- [ ] a", "a durable fact"} {
		if !strings.Contains(p, want) {
			t.Errorf("projection dropped %q", want)
		}
	}
	if len(p) >= len(md) {
		t.Errorf("projection (%d) is not smaller than the file (%d)", len(p), len(md))
	}
}

func TestProjectionCarriesRecall(t *testing.T) {
	md := "## Status\ncontinuing\n\n## " + RecalledSection + "\n- the auth module lives in internal/auth\n"
	if !strings.Contains(Projection(md, 0), "internal/auth") {
		t.Error("projection dropped the recalled section, which is the point of recalling")
	}
}

func TestProjectionOfNothing(t *testing.T) {
	if got := Projection("", 0); got != "" {
		t.Errorf("projection of an empty file = %q", got)
	}
}

// End to end: the worker writes nothing, and the loop still advances.
func TestLoopFoldsTheTailWithoutTheWorkerWriting(t *testing.T) {
	dir := t.TempDir()
	n := 0
	dispatch := func(_ context.Context, _ Iteration, prompt, _ string) (*streamjson.ResultEvent, string, error) {
		n++
		// The worker never touches loop.md.
		if n == 1 {
			return &streamjson.ResultEvent{Result: "```loop\nSTATUS: continuing\nTRIED:\n- did the first half\nNOTES:\n- a durable fact\n```"}, "s", nil
		}
		if !strings.Contains(prompt, "did the first half") {
			t.Errorf("iteration 2's prompt did not inline the memory:\n%s", prompt)
		}
		return &streamjson.ResultEvent{Result: "```loop\nSTATUS: done\nTRIED:\n- did the second half\n```"}, "s", nil
	}
	sum, err := Run(context.Background(), Options{Dir: dir, Charter: "c", Dispatch: dispatch})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Action != Done {
		t.Fatalf("action = %s, want done", sum.Action)
	}
	if sum.Iterations != 2 {
		t.Errorf("ran %d iterations, want 2", sum.Iterations)
	}
	md, _ := Read(dir)
	for _, want := range []string{"did the first half", "did the second half", "a durable fact"} {
		if !strings.Contains(md, want) {
			t.Errorf("loop.md lost %q:\n%s", want, md)
		}
	}
}

// A worker that ignores the block and edits the file is still correct, just
// more expensive; the loop must not stall on it.
func TestLoopFallsBackWhenThereIsNoTail(t *testing.T) {
	dir := t.TempDir()
	dispatch := func(_ context.Context, _ Iteration, _, _ string) (*streamjson.ResultEvent, string, error) {
		_ = Write(dir, "## Status\ndone\n\n## Tried\n- wrote the file the old way\n")
		return &streamjson.ResultEvent{Result: "no block here"}, "s", nil
	}
	sum, err := Run(context.Background(), Options{Dir: dir, Charter: "c", Dispatch: dispatch})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Action != Done {
		t.Errorf("action = %s, want done from the file the worker wrote", sum.Action)
	}
	md, _ := Read(dir)
	if !strings.Contains(md, "wrote the file the old way") {
		t.Error("the worker's direct write was lost")
	}
}
