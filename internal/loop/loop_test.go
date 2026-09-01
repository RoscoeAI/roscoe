package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/streamjson"
)

// fakeWorker is a Dispatch that writes whatever status the script says on each
// iteration, standing in for a worker rewriting loop.md.
type fakeWorker struct {
	dir      string
	statuses []string // one per iteration; the last repeats
	costs    []float64
	errs     []error
	prompts  []string // what the loop asked for, in order
	resumes  []string
	calls    int
}

func (f *fakeWorker) dispatch(_ context.Context, _ Iteration, prompt, resume string) (*streamjson.ResultEvent, string, error) {
	f.prompts = append(f.prompts, prompt)
	f.resumes = append(f.resumes, resume)
	i := f.calls
	f.calls++

	pick := func(n int) string {
		if n < len(f.statuses) {
			return f.statuses[n]
		}
		if len(f.statuses) == 0 {
			return StatusContinuing
		}
		return f.statuses[len(f.statuses)-1]
	}
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	cost := 0.0
	if i < len(f.costs) {
		cost = f.costs[i]
	}

	if err == nil {
		md := fmt.Sprintf("# charter\n\n## Status\n%s\n\n## Tried\n- iteration %d\n", pick(i), i+1)
		if werr := Write(f.dir, md); werr != nil {
			return nil, "", werr
		}
	}
	return &streamjson.ResultEvent{TotalCostUSD: cost, SessionID: "sess-1"}, "sess-1", err
}

func newRun(t *testing.T, f *fakeWorker, o Options) (*Summary, error) {
	t.Helper()
	dir := t.TempDir()
	f.dir = dir
	o.Dir = dir
	o.Charter = "do the thing"
	o.Dispatch = f.dispatch
	return Run(context.Background(), o)
}

func TestLoopStopsWhenWorkerReportsDone(t *testing.T) {
	f := &fakeWorker{statuses: []string{StatusContinuing, StatusContinuing, StatusDone}}
	sum, err := newRun(t, f, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.Action != Done {
		t.Errorf("action = %s, want done (reason %q)", sum.Action, sum.Reason)
	}
	if sum.Iterations != 3 {
		t.Errorf("ran %d iterations, want 3", sum.Iterations)
	}
	if f.calls != 3 {
		t.Errorf("dispatched %d times, want 3", f.calls)
	}
}

func TestLoopEscalatesWhenBlocked(t *testing.T) {
	f := &fakeWorker{statuses: []string{StatusBlocked}}
	sum, err := newRun(t, f, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.Action != Escalate {
		t.Errorf("action = %s, want escalate", sum.Action)
	}
	if sum.Iterations != 1 {
		t.Errorf("ran %d iterations, want 1", sum.Iterations)
	}
}

// The ceiling has to hold even when the worker never says it is finished, or
// an unbounded loop is one typo away.
func TestLoopStopsAtIterationCeiling(t *testing.T) {
	f := &fakeWorker{statuses: []string{StatusContinuing}}
	sum, err := newRun(t, f, Options{MaxIterations: 4})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.Iterations != 4 {
		t.Errorf("ran %d iterations, want the 4 it was capped at", sum.Iterations)
	}
	if sum.Action != Escalate {
		t.Errorf("action = %s, want escalate at the ceiling", sum.Action)
	}
	if !strings.Contains(sum.Reason, "ceiling") {
		t.Errorf("reason %q should say it hit the ceiling", sum.Reason)
	}
}

// Budget is the loop's to enforce: a judge that wants to keep going must not
// be able to spend past the ceiling.
func TestLoopStopsOnBudgetEvenIfTheJudgeWouldContinue(t *testing.T) {
	f := &fakeWorker{
		statuses: []string{StatusContinuing},
		costs:    []float64{0.4, 0.4, 0.4},
	}
	sum, err := newRun(t, f, Options{
		BudgetUSD:     1.0,
		MaxIterations: 50,
		Judge:         FixedJudge{D: Decision{Action: Continue}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.Action != Escalate {
		t.Errorf("action = %s, want escalate on budget", sum.Action)
	}
	if sum.Iterations != 3 {
		t.Errorf("stopped after %d iterations, want 3 (0.4 x 3 >= 1.0)", sum.Iterations)
	}
	if sum.SpentUSD < 1.0 {
		t.Errorf("spent %.2f, want at least the 1.00 budget", sum.SpentUSD)
	}
}

// One failure is worth a retry; a run whose worker keeps failing is a broken
// setup and must not burn a budget proving it.
func TestLoopRetriesOnceThenAbortsOnRepeatedFailure(t *testing.T) {
	boom := errors.New("worker exploded")
	f := &fakeWorker{errs: []error{boom, boom, boom, boom}}
	sum, err := newRun(t, f, Options{MaxIterations: 20, MaxConsecutiveErrors: 3})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the worker's error", err)
	}
	if sum.Action != Abort {
		t.Errorf("action = %s, want abort", sum.Action)
	}
	if sum.Iterations != 3 {
		t.Errorf("ran %d iterations, want 3 before giving up", sum.Iterations)
	}
}

func TestLoopRecoversFromAFailedIteration(t *testing.T) {
	f := &fakeWorker{
		statuses: []string{StatusContinuing, StatusDone},
		errs:     []error{errors.New("transient")},
	}
	sum, err := newRun(t, f, Options{MaxIterations: 10})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.Action != Done {
		t.Errorf("action = %s, want done after recovering", sum.Action)
	}
	if !strings.Contains(f.prompts[1], "failed with") {
		t.Errorf("the retry prompt should mention the failure, got %q", f.prompts[1])
	}
}

// Later iterations must resume the same session, or every iteration starts
// cold and loop.md is doing work the transcript should be doing.
func TestLoopResumesTheSameSession(t *testing.T) {
	f := &fakeWorker{statuses: []string{StatusContinuing, StatusContinuing, StatusDone}}
	if _, err := newRun(t, f, Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.resumes[0] != "" {
		t.Errorf("the first iteration resumed %q, want a fresh session", f.resumes[0])
	}
	for i, r := range f.resumes[1:] {
		if r != "sess-1" {
			t.Errorf("iteration %d resumed %q, want sess-1", i+2, r)
		}
	}
}

// The kernel prompt is deliberately identical every iteration: loop.md carries
// the state, so the prompt does not drift.
func TestKernelPromptIsStable(t *testing.T) {
	f := &fakeWorker{statuses: []string{StatusContinuing, StatusContinuing, StatusDone}}
	if _, err := newRun(t, f, Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Only the memory projection may differ; the instruction half must not.
	if invariantOf(f.prompts[0]) != invariantOf(f.prompts[1]) {
		t.Errorf("the instruction half drifted between iterations:\n%q\nvs\n%q",
			invariantOf(f.prompts[0]), invariantOf(f.prompts[1]))
	}
	for _, want := range []string{FileName, StatusDone, StatusBlocked, "do the thing"} {
		if !strings.Contains(f.prompts[0], want) {
			t.Errorf("the kernel prompt never mentions %q", want)
		}
	}
}

// Resuming a run must not throw away what earlier iterations learned.
func TestExistingLoopFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	const prior = "# charter\n\n## Status\ncontinuing\n\n## Notes\n- something learned earlier\n"
	if err := Write(dir, prior); err != nil {
		t.Fatal(err)
	}
	seeded, err := EnsureSeeded(dir, "a different charter")
	if err != nil {
		t.Fatal(err)
	}
	if seeded {
		t.Error("EnsureSeeded reported writing over an existing file")
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != prior {
		t.Errorf("loop.md was rewritten:\n%s", got)
	}
}

func TestSeedIsUsableAndParses(t *testing.T) {
	dir := t.TempDir()
	seeded, err := EnsureSeeded(dir, "  build the thing  ")
	if err != nil || !seeded {
		t.Fatalf("seeded=%v err=%v", seeded, err)
	}
	md, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ParseStatus(md) != StatusContinuing {
		t.Errorf("a fresh loop.md reads as %q", ParseStatus(md))
	}
	if !strings.Contains(md, "build the thing") {
		t.Error("the seed does not carry the charter")
	}
	for _, want := range []string{"## Status", "## Plan", "## Tried", "## Notes"} {
		if !strings.Contains(md, want) {
			t.Errorf("the seed is missing the %s section", want)
		}
	}
}

func TestRunRejectsBadOptions(t *testing.T) {
	f := &fakeWorker{}
	if _, err := Run(context.Background(), Options{Dir: t.TempDir(), Charter: "x"}); err == nil {
		t.Error("a run with no dispatch should fail")
	}
	if _, err := Run(context.Background(), Options{Charter: "x", Dispatch: f.dispatch}); err == nil {
		t.Error("a run with no directory should fail")
	}
	if _, err := Run(context.Background(), Options{Dir: t.TempDir(), Charter: "  ", Dispatch: f.dispatch}); err == nil {
		t.Error("a run with an empty charter should fail")
	}
}

func TestCancelledContextStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	f := &fakeWorker{dir: dir, statuses: []string{StatusContinuing}}
	sum, err := Run(ctx, Options{Dir: dir, Charter: "x", Dispatch: f.dispatch})
	if err == nil {
		t.Error("a cancelled run should report the cancellation")
	}
	if sum == nil || sum.Action != Abort {
		t.Errorf("summary = %+v, want an abort", sum)
	}
	if f.calls != 0 {
		t.Errorf("dispatched %d times after cancellation", f.calls)
	}
}

func TestParseStatus(t *testing.T) {
	cases := []struct{ md, want string }{
		{"## Status\ndone\n", StatusDone},
		{"## Status\nblocked\n", StatusBlocked},
		{"## Status\ncontinuing\n", StatusContinuing},
		{"## Status\n\n  DONE  \n", StatusDone},        // whitespace and case
		{"## Status\n**done**\n", StatusDone},          // a worker being markdown-ish
		{"## status\ndone\n", StatusDone},              // heading case
		{"# Charter\n\n## Status\ndone\n", StatusDone}, // not the first heading
		{"## Status\n## Plan\ndone\n", StatusContinuing},
		{"no headings at all", StatusContinuing},
		{"## Status\nfinished-ish\n", StatusContinuing}, // unrecognised is not done
		{"", StatusContinuing},
	}
	for _, tc := range cases {
		if got := ParseStatus(tc.md); got != tc.want {
			t.Errorf("ParseStatus(%q) = %q, want %q", tc.md, got, tc.want)
		}
	}
}

func TestSection(t *testing.T) {
	md := "# Charter\n\n## Status\ndone\n\n## Tried\n- one\n- two\n\n## Notes\n- a fact\n"
	if got := Section(md, "Tried"); got != "- one\n- two" {
		t.Errorf("Tried = %q", got)
	}
	if got := Section(md, "notes"); got != "- a fact" {
		t.Errorf("Notes = %q", got)
	}
	if got := Section(md, "Nonexistent"); got != "" {
		t.Errorf("a missing section = %q, want empty", got)
	}
}

func TestReadMissingFile(t *testing.T) {
	got, err := Read(t.TempDir())
	if err != nil {
		t.Fatalf("reading a missing loop.md should not error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestWriteCreatesTheDirectory(t *testing.T) {
	dir := t.TempDir() + "/nested/deeper"
	if err := Write(dir, "# x\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(dir)); err != nil {
		t.Errorf("loop.md was not created: %v", err)
	}
}

// A zero-byte loop.md is what a crashed write leaves behind. Treating it as
// existing memory hands the next worker a file with no status at all.
func TestEmptyLoopFileIsReseeded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	seeded, err := EnsureSeeded(dir, "the charter")
	if err != nil {
		t.Fatal(err)
	}
	if !seeded {
		t.Fatal("a zero-byte loop.md was treated as existing memory")
	}
	md, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "the charter") || ParseStatus(md) != StatusContinuing {
		t.Errorf("re-seeded file is not usable:\n%s", md)
	}
}

// loop.md is what a crashed run is recovered from, so a write must never be
// observable half-done.
func TestWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, "# first\n"); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, "# second, longer content\n"); err != nil {
		t.Fatal(err)
	}
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "# second, longer content\n" {
		t.Errorf("got %q", got)
	}
	// The temp file must not survive as litter beside loop.md.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != FileName {
			t.Errorf("Write left %q behind", e.Name())
		}
	}
}

// A turn the harness marked failed counts as a failed iteration, so a per-task
// budget set too low stops after MaxConsecutiveErrors instead of grinding to
// the ceiling.
func TestHarnessReportedFailureCountsAsAnError(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	dispatch := func(_ context.Context, _ Iteration, _, _ string) (*streamjson.ResultEvent, string, error) {
		calls++
		_ = Write(dir, "## Status\ncontinuing\n")
		return &streamjson.ResultEvent{IsError: true, Result: "budget exceeded\nmore detail"}, "s1", nil
	}
	sum, err := Run(context.Background(), Options{
		Dir: dir, Charter: "x", Dispatch: dispatch,
		MaxIterations: 20, MaxConsecutiveErrors: 3,
	})
	if err == nil {
		t.Fatal("a run of harness-reported failures should end in an error")
	}
	if sum.Action != Abort {
		t.Errorf("action = %s, want abort", sum.Action)
	}
	if calls != 3 {
		t.Errorf("dispatched %d times, want 3 before giving up", calls)
	}
	if !strings.Contains(sum.Reason, "budget exceeded") {
		t.Errorf("reason should carry the worker's own message, got %q", sum.Reason)
	}
	if strings.Contains(sum.Reason, "more detail") {
		t.Errorf("reason should be one line, got %q", sum.Reason)
	}
}

// The loop's transcript is never trimmed and `claude -p --resume` rebuilds the
// whole log into one request, so a long run eventually cannot resume at all.
// Repeated failure must drop the session and continue from loop.md rather than
// retry into the same wall until it aborts.
func TestRepeatedFailureRestartsCold(t *testing.T) {
	dir := t.TempDir()
	var resumes []string
	fail := errors.New("prompt is too long")
	calls := 0
	dispatch := func(_ context.Context, _ Iteration, _, resume string) (*streamjson.ResultEvent, string, error) {
		resumes = append(resumes, resume)
		calls++
		if calls <= 2 { // the first two attempts fail
			return &streamjson.ResultEvent{SessionID: "sess-1"}, "sess-1", fail
		}
		_ = Write(dir, "## Status\ndone\n")
		return &streamjson.ResultEvent{SessionID: "sess-2"}, "sess-2", nil
	}
	sum, err := Run(context.Background(), Options{
		Dir: dir, Charter: "x", Dispatch: dispatch,
		MaxIterations: 10, MaxConsecutiveErrors: 5,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.Action != Done {
		t.Errorf("action = %s, want done after recovering cold", sum.Action)
	}
	if len(resumes) < 3 {
		t.Fatalf("only %d dispatches", len(resumes))
	}
	if resumes[0] != "" {
		t.Errorf("first dispatch resumed %q, want a fresh session", resumes[0])
	}
	if resumes[1] != "sess-1" {
		t.Errorf("the first retry should keep the session, got %q", resumes[1])
	}
	if resumes[2] != "" {
		t.Errorf("the second retry should start cold, got %q", resumes[2])
	}
}

// A single failure is still worth retrying warm: most are transient and the
// transcript is worth keeping.
func TestOneFailureKeepsTheSession(t *testing.T) {
	f := &fakeWorker{
		statuses: []string{StatusContinuing, StatusDone},
		errs:     []error{errors.New("transient")},
	}
	if _, err := newRun(t, f, Options{MaxIterations: 10}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.resumes[1] != "sess-1" {
		t.Errorf("a single failure dropped the session (%q); it should retry warm", f.resumes[1])
	}
}

// Recall is written into loop.md before the worker runs, so the worker just
// reads its memory file and never learns a graph exists.
func TestRecallIsWrittenIntoTheMemoryFile(t *testing.T) {
	dir := t.TempDir()
	var sawInPrompt string
	dispatch := func(_ context.Context, _ Iteration, _, _ string) (*streamjson.ResultEvent, string, error) {
		md, _ := Read(dir)
		sawInPrompt = md
		_ = Write(dir, "## Status\ndone\n")
		return &streamjson.ResultEvent{SessionID: "s"}, "s", nil
	}
	var signalled []Action
	sum, err := Run(context.Background(), Options{
		Dir: dir, Charter: "x", Dispatch: dispatch,
		Recall: func(context.Context, Iteration) string { return "- the auth module lives in internal/auth" },
		Signal: func(_ context.Context, _ Iteration, d Decision) { signalled = append(signalled, d.Action) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Action != Done {
		t.Errorf("action = %s", sum.Action)
	}
	if !strings.Contains(sawInPrompt, "internal/auth") {
		t.Errorf("the worker did not see the recall in loop.md:\n%s", sawInPrompt)
	}
	if !strings.Contains(sawInPrompt, "## "+RecalledSection) {
		t.Error("recall was not put under its own heading")
	}
	if len(signalled) != 1 || signalled[0] != Done {
		t.Errorf("signalled %v, want one Done", signalled)
	}
}

// Memory is never in the hot path: a recall that fails, hangs, or says nothing
// must cost the run nothing at all.
func TestRecallFailureNeverBreaksTheLoop(t *testing.T) {
	for name, recall := range map[string]func(context.Context, Iteration) string{
		"empty":      func(context.Context, Iteration) string { return "" },
		"whitespace": func(context.Context, Iteration) string { return "   \n  " },
		"panic-free": nil,
	} {
		dir := t.TempDir()
		dispatch := func(_ context.Context, _ Iteration, _, _ string) (*streamjson.ResultEvent, string, error) {
			_ = Write(dir, "## Status\ndone\n")
			return &streamjson.ResultEvent{}, "", nil
		}
		sum, err := Run(context.Background(), Options{
			Dir: dir, Charter: "x", Dispatch: dispatch, Recall: recall,
		})
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if sum.Action != Done {
			t.Errorf("%s: action = %s, want done", name, sum.Action)
		}
		md, _ := Read(dir)
		if strings.Contains(md, "## "+RecalledSection) {
			t.Errorf("%s: an empty recall still wrote a heading:\n%s", name, md)
		}
	}
}

// Stale recall is worse than none, so the section is regenerated rather than
// accumulated the way Tried and Notes are.
func TestRecalledSectionIsNotPreserved(t *testing.T) {
	before := "## Status\ncontinuing\n\n## " + RecalledSection + "\n- old and stale\n\n## Notes\n- a real fact\n"
	after := "## Status\ncontinuing\n\n## Notes\n- a real fact\n"
	merged, restored := MergePreserving(before, after)
	if strings.Contains(merged, "old and stale") {
		t.Errorf("stale recall was preserved:\n%s", merged)
	}
	for _, e := range restored {
		if strings.Contains(e, "old and stale") {
			t.Errorf("restored stale recall %q", e)
		}
	}
}

// invariantOf strips the memory projection, leaving the half of the kernel
// prompt that must never change between iterations.
func invariantOf(prompt string) string {
	const open, close = "--- MEMORY (" + FileName + ") ---", "--- END MEMORY ---"
	i := strings.Index(prompt, open)
	if i < 0 {
		return prompt
	}
	j := strings.Index(prompt, close)
	if j < 0 {
		return prompt
	}
	return prompt[:i] + prompt[j+len(close):]
}
