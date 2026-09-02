package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"roscoe.sh/roscoe/internal/config"
)

// Fetcher copies host:remotePath (a directory) to localPath. The real one is
// scp -r; tests inject one that writes files.
type Fetcher func(ctx context.Context, host, remotePath, localPath string) error

// remoteRunsDir is where roscoe on a node keeps its ledgers, relative to the
// remote home. Nodes run with the default state dir; deploy pushes the config
// but not a state_dir of its own, so this is the path on every node deploy has
// touched. It is relative because scp speaks SFTP, which resolves a relative
// path against the home directory and does not expand $HOME (the first live
// fetch failed on exactly that); the ssh listing runs through a shell and
// gets the same directory as "$HOME/" + remoteRunsDir.
const remoteRunsDir = `.roscoe/runs`

// RemoteRuns lists the task ids that have a ledger on the node.
func RemoteRuns(ctx context.Context, n config.Node, run Runner) ([]string, error) {
	out, err := run(ctx, n.SSH, `ls -1 "$HOME/`+remoteRunsDir+`" 2>/dev/null; true`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			ids = append(ids, l)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// HomeTag is the note BringHome appends to a fetched ledger, so `sessions`
// can say which node a run happened on. The ledger's own node field says
// "local", because to the roscoe that wrote it, it was.
type HomeTag struct {
	TS   string `json:"ts"`
	Kind string `json:"kind"` // "fleet.home"
	Node string `json:"node"`
	SSH  string `json:"ssh"`
}

// BringHome copies the ledgers for ids from the node into runsDir, skipping
// any already here, and tags each with the node it came from. It returns the
// ids it fetched. One failed fetch does not stop the others; the first error
// is returned after all were tried.
func BringHome(ctx context.Context, n config.Node, ids []string, runsDir string, fetch Fetcher) ([]string, error) {
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return nil, err
	}
	var got []string
	var firstErr error
	for _, id := range ids {
		if id == "" || strings.ContainsAny(id, "/\\ ") || strings.HasPrefix(id, ".") {
			continue // a ledger dir name, not a path
		}
		local := filepath.Join(runsDir, id)
		if _, err := os.Stat(filepath.Join(local, "events.jsonl")); err == nil {
			continue // already home
		}
		if err := fetch(ctx, n.SSH, remoteRunsDir+"/"+id, local); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("fetch %s from %s: %w", id, n.Name, err)
			}
			continue
		}
		if err := tagHome(filepath.Join(local, "events.jsonl"), n); err != nil && firstErr == nil {
			firstErr = err
		}
		got = append(got, id)
	}
	return got, firstErr
}

func tagHome(ledger string, n config.Node) error {
	f, err := os.OpenFile(ledger, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("tag %s: %w", ledger, err)
	}
	defer f.Close()
	b, _ := json.Marshal(HomeTag{TS: time.Now().UTC().Format(time.RFC3339Nano), Kind: "fleet.home", Node: n.Name, SSH: n.SSH})
	_, err = f.Write(append(b, '\n'))
	return err
}
