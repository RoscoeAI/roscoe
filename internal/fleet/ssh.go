package fleet

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// SSH is the real Runner: the operator's ssh, their aliases and keys, batch
// mode so a missing key fails fast instead of prompting under a TUI.
func SSH(ctx context.Context, host, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		host, command)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("ssh %s: %w: %s", host, err, lastLine(out.String()))
	}
	return out.String(), nil
}

// SCP is the real Copier.
func SCP(ctx context.Context, host, local, remote string) error {
	cmd := exec.CommandContext(ctx, "scp",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		"-q", local, host+":"+remote)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scp %s -> %s:%s: %w: %s", local, host, remote, err, lastLine(out.String()))
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return ""
	}
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 160 {
		l = l[:160] + "…"
	}
	return l
}
