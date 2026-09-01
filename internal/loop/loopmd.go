// Package loop is roscoe's supervisor: the deterministic cycle of dispatch,
// read the result, judge, dispatch again. The loop is Go rather than a prompt
// so it survives crashes, works identically for either harness, and leaves its
// state on disk where you can read it. See ARCHITECTURE.md "Autonomy".
package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileName is the working memory each iteration reads and rewrites. It lives
// in the task's working directory, so it is diffable, greppable, and editable
// by hand mid-run.
const FileName = "loop.md"

// Status values a worker may write. Anything else is read as StatusContinuing,
// because a worker that garbles its own status should be asked to carry on,
// not silently treated as finished.
const (
	StatusContinuing = "continuing"
	StatusDone       = "done"
	StatusBlocked    = "blocked"
)

// Path is where loop.md lives for a task.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Seed is the initial loop.md: the charter, an empty plan, and a status the
// worker is expected to maintain.
func Seed(charter string) string {
	return fmt.Sprintf(`# %s

## Status
%s

## Plan
- [ ] read the charter and write a plan here

## Tried
_nothing yet_

## Notes
_durable facts about this codebase go here; they outlive the run_
`, strings.TrimSpace(charter), StatusContinuing)
}

// Read returns loop.md's contents, or "" when it does not exist yet.
func Read(dir string) (string, error) {
	b, err := os.ReadFile(Path(dir))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", FileName, err)
	}
	return string(b), nil
}

// Write replaces loop.md atomically. A truncating write would leave the run's
// only durable memory half-written if anything died mid-write, and this file
// is precisely what a crashed run is meant to be recovered from.
func Write(dir, content string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", FileName, err)
	}
	tmp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp %s: %w", FileName, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", FileName, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", FileName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", FileName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", FileName, err)
	}
	if err := os.Rename(tmpName, Path(dir)); err != nil {
		return fmt.Errorf("rename %s into place: %w", FileName, err)
	}
	return nil
}

// EnsureSeeded writes the seed file if there is not one already, and reports
// whether it wrote. An existing file is never overwritten: resuming a run must
// not throw away what earlier iterations learned.
func EnsureSeeded(dir, charter string) (bool, error) {
	// Stat succeeds on a zero-byte file, which is what a crashed or truncated
	// write leaves behind. An empty loop.md is not memory worth keeping, and
	// treating it as one hands the worker a file with no status at all.
	if fi, err := os.Stat(Path(dir)); err == nil {
		if fi.Size() > 0 {
			return false, nil
		}
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat %s: %w", FileName, err)
	}
	return true, Write(dir, Seed(charter))
}

// ParseStatus reads the first non-empty line under the "## Status" heading.
// A missing or unrecognised status reads as continuing: the loop's stop
// conditions are the authority on stopping, not a typo in a markdown file.
func ParseStatus(md string) string {
	lines := strings.Split(md, "\n")
	for i, line := range lines {
		if !isHeading(line, "status") {
			continue
		}
		for _, next := range lines[i+1:] {
			t := strings.TrimSpace(next)
			if t == "" {
				continue
			}
			if strings.HasPrefix(t, "#") { // an empty Status section
				break
			}
			switch strings.ToLower(strings.Trim(t, "*_`.- ")) {
			case StatusDone:
				return StatusDone
			case StatusBlocked:
				return StatusBlocked
			}
			return StatusContinuing
		}
		break
	}
	return StatusContinuing
}

func isHeading(line, name string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "#") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(strings.TrimLeft(t, "# ")), name)
}

// Section returns the body under a heading, or "" when there is none. The
// judge uses it to read the plan and what has been tried without needing the
// whole file.
func Section(md, name string) string {
	lines := strings.Split(md, "\n")
	for i, line := range lines {
		if !isHeading(line, name) {
			continue
		}
		var body []string
		for _, next := range lines[i+1:] {
			if strings.HasPrefix(strings.TrimSpace(next), "#") {
				break
			}
			body = append(body, next)
		}
		return strings.TrimSpace(strings.Join(body, "\n"))
	}
	return ""
}
