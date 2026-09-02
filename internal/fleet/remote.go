package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"roscoe.sh/roscoe/internal/config"
)

// RemoteOpts is what a task carries to another node. The task id is minted
// on the control plane so the operator can find the run afterwards; the
// node's ledger and sessions live on the node.
type RemoteOpts struct {
	TaskID  string
	Dir     string // working directory on the node; empty means a fresh one under ~/.roscoe/work
	Harness string // "" keeps the node's tiers.middle.harness
}

// WorkDir is where the task runs on the node. Nodes have no checkout of the
// control plane's cwd, so the default is a directory of the task's own, and
// --dir names an existing one there.
func (o RemoteOpts) WorkDir() string {
	if o.Dir != "" {
		return shellQuote(o.Dir)
	}
	return `"$HOME/.roscoe/work/"` + shellQuote(o.TaskID)
}

// DisplayDir is WorkDir for a person: no shell quoting, ~ for the home.
func (o RemoteOpts) DisplayDir() string {
	if o.Dir != "" {
		return o.Dir
	}
	return "~/.roscoe/work/" + o.TaskID
}

// RemoteRun is the shell command that runs one task on a node: the same
// `roscoe run` the operator would type there, with the user PATH the
// installers need and the working directory created first.
func RemoteRun(prompt string, o RemoteOpts) string {
	dir := o.WorkDir()
	cmd := userPath + "mkdir -p " + dir + " && cd " + dir +
		" && roscoe run " + shellQuote(prompt) + " --task-id " + shellQuote(o.TaskID)
	if o.Harness != "" {
		cmd += " --harness " + shellQuote(o.Harness)
	}
	return cmd
}

// Exec runs a command on a node with this process's stdin, stdout and stderr
// attached, so the worker's narration streams back and the operator's keys
// (Esc to interrupt, a redirect line) reach it. With tty, ssh allocates one
// on the node so the remote roscoe behaves as it would in a terminal there;
// without, stdin is closed (-n) so a piped caller's input is not swallowed.
// The exit code is the remote roscoe's.
func Exec(ctx context.Context, n config.Node, command string, tty bool) int {
	cmd := exec.CommandContext(ctx, "ssh", SSHArgs(n, command, tty)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh %s: %v\n", n.SSH, err)
		return 1
	}
	return 0
}

// SSHArgs is what Exec passes to ssh, exposed for tests.
func SSHArgs(n config.Node, command string, tty bool) []string {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "-o", "StrictHostKeyChecking=accept-new"}
	if tty {
		args = append(args, "-t")
	} else {
		args = append(args, "-n")
	}
	return append(args, n.SSH, command)
}
