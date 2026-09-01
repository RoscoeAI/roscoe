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

// Write replaces loop.md.
func Write(dir, content string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", FileName, err)
	}
	if err := os.WriteFile(Path(dir), []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", FileName, err)
	}
	return nil
}

// EnsureSeeded writes the seed file if there is not one already, and reports
// whether it wrote. An existing file is never overwritten: resuming a run must
// not throw away what earlier iterations learned.
func EnsureSeeded(dir, charter string) (bool, error) {
	if _, err := os.Stat(Path(dir)); err == nil {
		return false, nil
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
