package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"roscoe.sh/roscoe/internal/config"
)

func fakeFetch(t *testing.T, fail string) (Fetcher, *[]string) {
	var calls []string
	return func(ctx context.Context, host, remote, local string) error {
		calls = append(calls, host+":"+remote)
		if strings.HasSuffix(remote, "/"+fail) {
			return errors.New("scp: No such file or directory")
		}
		if err := os.MkdirAll(local, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(local, "events.jsonl"),
			[]byte(`{"ts":"2026-09-02T03:54:26Z","node":"local","task":"`+filepath.Base(remote)+`","seq":1,"event":{"type":"system","subtype":"init","session_id":"s1"}}`+"\n"), 0o644)
	}, &calls
}

// A fetched ledger lands where the local ones live, is tagged with its node,
// and is not fetched twice; a run that is already home is left alone.
func TestBringHomeFetchesTagsAndSkips(t *testing.T) {
	runs := t.TempDir()
	n := config.Node{Name: "roscoe", SSH: "roscoe-ts"}
	if err := os.MkdirAll(filepath.Join(runs, "task-here"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runs, "task-here", "events.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fetch, calls := fakeFetch(t, "")
	got, err := BringHome(context.Background(), n, []string{"task-here", "task-remote", "../evil", ".DS_Store"}, runs, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "task-remote" {
		t.Errorf("fetched %v, want only the one not here (and nothing path-like)", got)
	}
	// Home-relative, never $HOME: scp is SFTP and does not expand variables.
	if len(*calls) != 1 || (*calls)[0] != "roscoe-ts:.roscoe/runs/task-remote" {
		t.Errorf("calls = %v", *calls)
	}
	// The local one was not touched.
	if b, _ := os.ReadFile(filepath.Join(runs, "task-here", "events.jsonl")); string(b) != "{}\n" {
		t.Errorf("a ledger that was already home was rewritten: %q", b)
	}
	// The fetched one ends with the home tag.
	f, err := os.Open(filepath.Join(runs, "task-remote", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		last = sc.Text()
	}
	var tag HomeTag
	if json.Unmarshal([]byte(last), &tag) != nil || tag.Kind != "fleet.home" || tag.Node != "roscoe" || tag.SSH != "roscoe-ts" || tag.TS == "" {
		t.Errorf("last line is not a home tag: %s", last)
	}
	// Fetching again is a no-op.
	got, _ = BringHome(context.Background(), n, []string{"task-remote"}, runs, fetch)
	if len(got) != 0 || len(*calls) != 1 {
		t.Errorf("second BringHome refetched: got %v calls %v", got, *calls)
	}
}

// One bad ledger must not stop the rest coming home.
func TestBringHomeContinuesPastAFailure(t *testing.T) {
	fetch, _ := fakeFetch(t, "task-b")
	got, err := BringHome(context.Background(), config.Node{Name: "n", SSH: "n-ts"}, []string{"task-a", "task-b", "task-c"}, t.TempDir(), fetch)
	if strings.Join(got, ",") != "task-a,task-c" {
		t.Errorf("fetched %v", got)
	}
	if err == nil || !strings.Contains(err.Error(), "task-b") {
		t.Errorf("err = %v, want it to name the one that failed", err)
	}
}

func TestRemoteRunsParsesLs(t *testing.T) {
	run := func(ctx context.Context, host, cmd string) (string, error) {
		if !strings.Contains(cmd, `ls -1 "$HOME/.roscoe/runs"`) {
			t.Errorf("unexpected command %q", cmd)
		}
		return "task-b\n\ntask-a\n", nil
	}
	ids, err := RemoteRuns(context.Background(), config.Node{SSH: "x"}, run)
	if err != nil || strings.Join(ids, ",") != "task-a,task-b" {
		t.Errorf("ids = %v, %v", ids, err)
	}
	// An empty runs dir (or none) is no runs, not an error.
	ids, err = RemoteRuns(context.Background(), config.Node{SSH: "x"}, func(context.Context, string, string) (string, error) { return "", nil })
	if err != nil || len(ids) != 0 {
		t.Errorf("empty = %v, %v", ids, err)
	}
}
