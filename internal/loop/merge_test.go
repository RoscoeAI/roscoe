package loop

import (
	"context"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/streamjson"
)

const withMemory = `# charter

## Status
continuing

## Plan
- [ ] step one
- [ ] step two

## Tried
- iteration 1: tried the fast path, it deadlocked under load
- iteration 2: tried the slow path, it worked

## Notes
- the build needs CGO_ENABLED=0
- the API rejects requests over 1MB
`

func triedOf(t *testing.T, md string) string { t.Helper(); return Section(md, "Tried") }
func notesOf(t *testing.T, md string) string { t.Helper(); return Section(md, "Notes") }

// The bug this exists for: a worker rewrites the whole file and drops what
// earlier iterations learned.
func TestDroppedEntriesAreRestored(t *testing.T) {
	after := `# charter

## Status
done

## Plan
- [x] step one
- [x] step two

## Tried
- iteration 3: finished it

## Notes
- the API rejects requests over 1MB
`
	merged, restored := MergePreserving(withMemory, after)
	if len(restored) != 3 {
		t.Fatalf("restored %d entries, want 3 (2 tried + 1 note): %q", len(restored), restored)
	}
	for _, want := range []string{"deadlocked under load", "the slow path, it worked", "CGO_ENABLED=0"} {
		if !strings.Contains(merged, want) {
			t.Errorf("merged file lost %q:\n%s", want, merged)
		}
	}
	// What the worker actually wrote this turn must survive too.
	if !strings.Contains(triedOf(t, merged), "iteration 3: finished it") {
		t.Error("the new entry was lost")
	}
	// Status and Plan are the worker's to replace, not accumulate.
	if ParseStatus(merged) != StatusDone {
		t.Errorf("status = %q, want the worker's new done", ParseStatus(merged))
	}
	if strings.Contains(Section(merged, "Plan"), "- [ ] step one") {
		t.Error("the old unchecked plan came back; Plan is replaced, not merged")
	}
}

func TestNothingRestoredWhenNothingLost(t *testing.T) {
	after := strings.Replace(withMemory, "## Status\ncontinuing", "## Status\ndone", 1)
	merged, restored := MergePreserving(withMemory, after)
	if len(restored) != 0 {
		t.Errorf("restored %q from an unchanged file", restored)
	}
	if merged != after {
		t.Errorf("file was rewritten unnecessarily:\n%s", merged)
	}
}

// Reordering is not loss, and must not duplicate.
func TestReorderingIsNotLoss(t *testing.T) {
	after := `# charter

## Status
continuing

## Tried
- iteration 2: tried the slow path, it worked
- iteration 1: tried the fast path, it deadlocked under load

## Notes
- the API rejects requests over 1MB
- the build needs CGO_ENABLED=0
`
	merged, restored := MergePreserving(withMemory, after)
	if len(restored) != 0 {
		t.Errorf("reordering was treated as loss: %q", restored)
	}
	if n := strings.Count(merged, "deadlocked under load"); n != 1 {
		t.Errorf("entry appears %d times, want 1", n)
	}
}

// A worker expanding an entry has kept it; resurrecting the older, shorter
// version would make the file grow with near-duplicates.
func TestExpandingAnEntryIsNotLoss(t *testing.T) {
	after := `# charter

## Status
continuing

## Tried
- iteration 1: tried the fast path, it deadlocked under load, root cause was the mutex
- iteration 2: tried the slow path, it worked

## Notes
- the build needs CGO_ENABLED=0
- the API rejects requests over 1MB
`
	_, restored := MergePreserving(withMemory, after)
	if len(restored) != 0 {
		t.Errorf("expanding an entry was treated as loss: %q", restored)
	}
}

// Shortening loses information, so the longer original comes back.
func TestShorteningAnEntryRestoresIt(t *testing.T) {
	after := `# charter

## Status
continuing

## Tried
- iteration 1: tried the fast path

## Notes
- the build needs CGO_ENABLED=0
- the API rejects requests over 1MB
`
	merged, restored := MergePreserving(withMemory, after)
	if len(restored) == 0 {
		t.Fatal("shortening an entry lost detail and was not restored")
	}
	if !strings.Contains(merged, "deadlocked under load") {
		t.Error("the detail that was dropped did not come back")
	}
}

// A worker that truncates the file entirely must not erase the run's memory.
func TestTruncatedFileKeepsTheOldOne(t *testing.T) {
	merged, restored := MergePreserving(withMemory, "   \n")
	if len(restored) == 0 {
		t.Error("truncation was not reported")
	}
	if merged != withMemory {
		t.Error("truncation erased the memory")
	}
}

// A section the worker deleted outright is the worst case, and the one most
// likely to happen when a worker "tidies up".
func TestDeletedSectionIsRebuilt(t *testing.T) {
	after := "# charter\n\n## Status\ncontinuing\n\n## Tried\n- iteration 3: did a thing\n"
	merged, restored := MergePreserving(withMemory, after)
	if len(restored) < 2 {
		t.Fatalf("restored %d, want both notes back", len(restored))
	}
	notes := notesOf(t, merged)
	for _, want := range []string{"CGO_ENABLED=0", "over 1MB"} {
		if !strings.Contains(notes, want) {
			t.Errorf("Notes lost %q; section is:\n%s", want, notes)
		}
	}
	if !strings.Contains(merged, "iteration 3: did a thing") {
		t.Error("the worker's new entry was lost while rebuilding")
	}
	if ParseStatus(merged) != StatusContinuing {
		t.Errorf("status = %q", ParseStatus(merged))
	}
}

// The seed's own filler must never be preserved over real content, or every
// run carries "_nothing yet_" forever.
func TestSeedPlaceholdersAreNotPreserved(t *testing.T) {
	seed := Seed("do the thing")
	after := "# do the thing\n\n## Status\ncontinuing\n\n## Tried\n- iteration 1: real work\n\n## Notes\n- a real fact\n"
	merged, restored := MergePreserving(seed, after)
	for _, e := range restored {
		if strings.Contains(strings.ToLower(e), "nothing yet") {
			t.Errorf("restored the seed placeholder %q", e)
		}
	}
	if strings.Contains(merged, "nothing yet") {
		t.Errorf("placeholder survived into the merged file:\n%s", merged)
	}
}

func TestMultiLineEntriesSurviveWhole(t *testing.T) {
	before := "## Tried\n- iteration 1: a long finding\n  that wrapped onto a second line\n  and a third\n"
	after := "## Tried\n- iteration 2: something else\n"
	merged, restored := MergePreserving(before, after)
	if len(restored) != 1 {
		t.Fatalf("restored %d entries, want 1 whole entry: %q", len(restored), restored)
	}
	for _, want := range []string{"a long finding", "wrapped onto a second line", "and a third"} {
		if !strings.Contains(merged, want) {
			t.Errorf("continuation line %q was lost", want)
		}
	}
}

func TestMergeWithNoPriorFile(t *testing.T) {
	after := "## Status\ndone\n"
	merged, restored := MergePreserving("", after)
	if len(restored) != 0 || merged != after {
		t.Errorf("a first iteration should merge to exactly what it wrote")
	}
}

func TestSplitEntries(t *testing.T) {
	got := splitEntries("- one\n- two\n  continued\n\n- three")
	if len(got) != 3 {
		t.Fatalf("got %d entries: %q", len(got), got)
	}
	if !strings.Contains(got[1], "continued") {
		t.Errorf("continuation was split off: %q", got[1])
	}
	if n := len(splitEntries("")); n != 0 {
		t.Errorf("empty body gave %d entries", n)
	}
	if n := len(splitEntries("_nothing yet_")); n != 0 {
		t.Errorf("placeholder gave %d entries", n)
	}
}

// End to end: the loop itself must enforce this, not just the helper.
func TestLoopRestoresMemoryTheWorkerDropped(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, withMemory); err != nil {
		t.Fatal(err)
	}
	// A worker that "tidies up" and keeps only its own note.
	dispatch := func(_ context.Context, _ Iteration, _, _ string) (*streamjson.ResultEvent, string, error) {
		_ = Write(dir, "# charter\n\n## Status\ndone\n\n## Tried\n- iteration 3: tidied up\n")
		return &streamjson.ResultEvent{SessionID: "s"}, "s", nil
	}
	var restoredNote bool
	sum, err := Run(context.Background(), Options{
		Dir: dir, Charter: "charter", Dispatch: dispatch,
		OnIteration: func(it Iteration, d Decision) {
			if strings.Contains(it.LoopMD, "CGO_ENABLED=0") {
				restoredNote = true
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Action != Done {
		t.Errorf("action = %s, want done", sum.Action)
	}
	if !restoredNote {
		t.Error("the judge saw a loop.md with the dropped note still missing")
	}
	onDisk, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CGO_ENABLED=0", "over 1MB", "deadlocked under load", "iteration 3: tidied up"} {
		if !strings.Contains(onDisk, want) {
			t.Errorf("loop.md on disk lost %q:\n%s", want, onDisk)
		}
	}
}

func TestSetSection(t *testing.T) {
	md := "# t\n\n## Status\ncontinuing\n\n## Tried\n- old\n\n## Notes\n- n\n"
	got := setSection(md, "Tried", "- new")
	if Section(got, "Tried") != "- new" {
		t.Errorf("Tried = %q", Section(got, "Tried"))
	}
	if Section(got, "Notes") != "- n" || ParseStatus(got) != StatusContinuing {
		t.Errorf("setSection disturbed its neighbours:\n%s", got)
	}
	added := setSection("# t\n\n## Status\ndone\n", "Notes", "- fresh")
	if Section(added, "Notes") != "- fresh" {
		t.Errorf("a missing section was not appended:\n%s", added)
	}
}
