//go:build darwin

package accounts

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// MacKeychain is the real store: the security tool against the login
// keychain. Items are generic passwords under the service named in the
// token_ref, account "roscoe".
type MacKeychain struct{}

const keychainAccount = "roscoe"

// ErrLocked is what a non-GUI session (ssh into a Mac) gets: the keychain
// exists but will not talk without a window to ask in. Nodes use env: refs
// for that reason.
var ErrLocked = errors.New("locked (no GUI session; use an env: ref on this machine)")

// Has runs find-generic-password without -w, so the secret is never read.
func (MacKeychain) Has(service string) (bool, error) {
	cmd := exec.Command("security", "find-generic-password", "-s", service, "-a", keychainAccount)
	cmd.Stdout = nil
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return classify(err, stderr.String())
}

// Get reads the token, to memory, for a worker's environment.
func (MacKeychain) Get(service string) (string, error) {
	cmd := exec.Command("security", "find-generic-password", "-s", service, "-a", keychainAccount, "-w")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		has, cerr := classify(err, stderr.String())
		if cerr != nil {
			return "", cerr
		}
		if !has {
			return "", errors.New("not found")
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// SetArgs is the command that stores a token under service by prompting for
// it on the terminal. The prompt only happens when -w is the LAST argument
// with no value (the man page's "put at end of command to be prompted");
// leave -w out and security stores an empty password without a word, which
// is exactly what happened once. -U updates an existing item.
func SetArgs(service string) []string {
	return []string{"add-generic-password", "-U", "-s", service, "-a", keychainAccount, "-l", "roscoe: " + service, "-w"}
}

// ErrEmpty is Set finding nothing behind the item it just wrote: the paste
// did not land (an empty line at the prompt, or a terminal that swallowed
// it), so the item is removed rather than left looking like a token.
var ErrEmpty = errors.New("nothing was stored; run the command again and paste the token at the prompt")

// Set stores a token interactively via SetArgs, then proves something is
// there. The check reads the secret to memory and compares its length only.
func (MacKeychain) Set(service string) error {
	cmd := exec.Command("security", SetArgs(service)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("security add-generic-password: %w", err)
	}
	got, err := MacKeychain{}.Get(service)
	if err != nil {
		return fmt.Errorf("stored, but reading it back failed: %w", err)
	}
	if got == "" {
		_ = MacKeychain{}.Delete(service)
		return ErrEmpty
	}
	return nil
}

// Delete removes the item, so an empty write does not masquerade as a token.
func (MacKeychain) Delete(service string) error {
	cmd := exec.Command("security", "delete-generic-password", "-s", service, "-a", keychainAccount)
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}

// classify turns the security tool's exit into (present, error). 44 is
// errSecItemNotFound; 36 is errSecInteractionNotAllowed, the locked case.
func classify(err error, stderr string) (bool, error) {
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false, err
	}
	switch ee.ExitCode() {
	case 44:
		return false, nil
	case 36:
		return false, ErrLocked
	}
	if strings.Contains(stderr, "could not be found") {
		return false, nil
	}
	if strings.Contains(stderr, "User interaction is not allowed") {
		return false, ErrLocked
	}
	return false, fmt.Errorf("security exit %d: %s", ee.ExitCode(), strings.TrimSpace(stderr))
}
