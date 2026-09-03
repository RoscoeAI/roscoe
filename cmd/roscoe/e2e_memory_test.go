package main

// roscoe memory, with graphify absent and with a stand-in on PATH that
// honours the five verbs the memory package uses.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFakeGraphify puts a graphify on the world's PATH. It logs every
// argv line and produces the files the real one would: a graph on update or
// extract, an answer on query, a memory entry on save-result, LESSONS.md on
// reflect.
func (w *world) installFakeGraphify() string {
	log := filepath.Join(filepath.Dir(w.bin), "graphify-argv")
	w.fake("graphify", `#!/bin/sh
printf '%s ' "$@" | tr '\n' ' ' >> "`+log+`"; echo >> "`+log+`"
verb="$1"; shift
last=""; out=""; memdir=""
for a in "$@"; do
  case "$last" in --out) out="$a";; --memory-dir) memdir="$a";; esac
  last="$a"
done
case "$verb" in
  update|extract)
    mkdir -p graphify-out && echo '{"nodes":[{"id":"n1","label":"thing"}],"edges":[]}' > graphify-out/graph.json; exit 0;;
  query)
    echo "recalled: $1 (from the graph)"; exit 0;;
  save-result)
    mkdir -p "$memdir" && echo "$@" >> "$memdir/results.jsonl"; exit 0;;
  reflect)
    echo "# Lessons" > "$out"; echo "- keep tests fast" >> "$out"; exit 0;;
esac
echo "fake graphify: unknown verb $verb" >&2; exit 1
`)
	return log
}

func graphifyCalls(log string) []string {
	var out []string
	for _, l := range strings.Split(readFile(log), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestE2EMemoryWithoutGraphify(t *testing.T) {
	w := newWorld(t)
	w.init()
	expect(t, w.run("", "memory", "bogus"), 2, `unknown subcommand "bogus"`)
	expect(t, w.run("", "memory", "status"), 0, "graphify  not installed", "(none yet)", "Install graphify to turn this on")
	expect(t, w.run("", "memory", "build"), 1, "graphify is not on PATH")
	expect(t, w.run("", "memory", "query", "what"), 1, "nothing to query yet", "roscoe memory status")
	expect(t, w.run("", "memory", "query"), 2, `usage: roscoe memory query`)
	expect(t, w.run("", "memory", "reflect"), 0, "nothing recorded yet")
	// Disabled memory says so rather than reaching for the binary.
	expect(t, w.run("", "config", "set", "memory.enabled", "false"), 0)
	expect(t, w.run("", "memory", "status"), 0, "(disabled)")
	expect(t, w.run("", "memory", "build"), 1, "memory is disabled")
}

func TestE2EMemoryWithGraphify(t *testing.T) {
	w := newWorld(t)
	w.init()
	log := w.installFakeGraphify()

	expect(t, w.run("", "memory", "status"), 0, "graphify  on PATH", "(none yet)", "No graph yet. Build one: roscoe memory build")

	r := w.run("", "memory", "build")
	expect(t, r, 0, "[memory] incremental update of "+w.cwd, "[memory] done in", "graph.json")
	calls := graphifyCalls(log)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "update "+w.cwd) {
		t.Fatalf("graphify calls after build = %v", calls)
	}
	graph := filepath.Join(w.home, ".roscoe", "graph")
	matches, _ := filepath.Glob(filepath.Join(graph, "*", "graphify-out", "graph.json"))
	if len(matches) != 1 {
		t.Fatalf("graph not written under state_dir: %v", matches)
	}
	// --full asks for the model-backed extraction; a corpus can be named.
	expect(t, w.run("", "memory", "build", "--full", w.home), 0, "[memory] full extraction of "+w.home)
	if calls = graphifyCalls(log); !strings.HasPrefix(calls[1], "extract "+w.home) {
		t.Errorf("full build call = %q", calls[1])
	}

	r = w.run("", "memory", "status")
	expect(t, r, 0, "graphify  on PATH")
	for _, line := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(line, "graph ") && strings.Contains(line, "(none yet)") {
			t.Errorf("status still says no graph after a build: %q", line)
		}
	}
	if strings.Contains(r.stdout, "No graph yet") {
		t.Errorf("status still offers to build:\n%s", r.stdout)
	}

	r = w.run("", "memory", "query", "where", "is", "the", "thing")
	expect(t, r, 0, "recalled: where is the thing (from the graph)")
	calls = graphifyCalls(log)
	if len(calls) != 3 {
		t.Fatalf("graphify calls after query = %v", calls)
	}
	q := calls[2]
	for _, want := range []string{"query where is the thing", "--budget 1200", "--graph " + matches[0]} {
		if !strings.Contains(q, want) {
			t.Errorf("query call %q lacks %q", q, want)
		}
	}
	// Nothing recorded yet, so reflect has nothing to distil.
	expect(t, w.run("", "memory", "reflect"), 0, "nothing recorded yet")
}

// A loop with a graph present recalls before dispatch, writes what it
// recalled into loop.md for the worker, reports whether it helped, and
// distils lessons at the end.
func TestE2ELoopUsesMemory(t *testing.T) {
	w := newWorld(t)
	w.init()
	log := w.installFakeGraphify()
	expect(t, w.run("", "memory", "build"), 0)

	r := w.run("", "loop", "Find the thing", "--once")
	expect(t, r, 0, "[memory] "+filepath.Join(w.home, ".roscoe", "graph"), "[iteration 1] ", "[memory] lessons: ")
	starts := w.claudeStarts()
	if len(starts) != 1 || !strings.Contains(starts[0], "recalled: Find the thing (from the graph)") {
		t.Errorf("the worker's prompt does not carry the recall:\n%v", starts)
	}
	md := readFile(filepath.Join(w.cwd, "loop.md"))
	if !strings.Contains(md, "recalled: Find the thing") {
		t.Errorf("loop.md does not carry the recall for the worker:\n%s", md)
	}
	var sawSave, sawReflect bool
	for _, c := range graphifyCalls(log) {
		if strings.HasPrefix(c, "save-result ") && strings.Contains(c, "--outcome useful") && strings.Contains(c, "--question Find the thing") {
			sawSave = true
		}
		if strings.HasPrefix(c, "reflect ") && strings.Contains(c, "--out ") {
			sawReflect = true
		}
	}
	if !sawSave || !sawReflect {
		t.Errorf("save-result=%v reflect=%v in graphify calls:\n%s", sawSave, sawReflect, strings.Join(graphifyCalls(log), "\n"))
	}
	lessons, _ := filepath.Glob(filepath.Join(w.home, ".roscoe", "graph", "*", "LESSONS.md"))
	if len(lessons) != 1 {
		t.Errorf("LESSONS.md not written: %v", lessons)
	}
	if _, err := os.Stat(filepath.Join(w.cwd, "graphify-out")); err == nil {
		t.Error("graphify ran in the project directory instead of the graph directory")
	}
}
