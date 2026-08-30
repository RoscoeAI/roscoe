package worker

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FindSession locates sessionID's transcript under configDir/projects/*/,
// returning its absolute path. configDir "" means the user's default
// claude home (~/.claude), which is where interactive sessions live.
func FindSession(configDir, sessionID string) (string, error) {
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("worker: resolve home: %w", err)
		}
		configDir = filepath.Join(home, ".claude")
	}
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return "", fmt.Errorf("worker: search sessions: %w", err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("worker: session %s not found under %s/projects", sessionID, configDir)
	}
	return matches[0], nil
}

// importSession copies a session transcript into destConfigDir, preserving
// the project-directory name (it encodes the session's original cwd, which
// claude uses to associate the transcript).
func importSession(srcPath, destConfigDir string) error {
	projectDir := filepath.Base(filepath.Dir(srcPath))
	destDir := filepath.Join(destConfigDir, "projects", projectDir)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("worker: create project dir: %w", err)
	}
	dest := filepath.Join(destDir, filepath.Base(srcPath))
	if _, err := os.Stat(dest); err == nil {
		return nil // already imported (re-resume)
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("worker: open source transcript: %w", err)
	}
	defer src.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("worker: create transcript copy: %w", err)
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return fmt.Errorf("worker: copy transcript: %w", err)
	}
	return out.Close()
}
