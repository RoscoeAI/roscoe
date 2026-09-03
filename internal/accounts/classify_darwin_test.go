//go:build darwin

package accounts

import (
	"errors"
	"os/exec"
	"testing"
)

func exitWith(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+itoa(code)).Run()
	if err == nil {
		t.Fatalf("exit %d did not error", code)
	}
	return err
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// The security tool's exits: 44 is not found, 36 is a locked keychain (no
// GUI session), anything else is an error worth showing; the stderr text
// is a fallback for versions that exit differently.
func TestClassify(t *testing.T) {
	if has, err := classify(nil, ""); !has || err != nil {
		t.Errorf("nil error = %v, %v", has, err)
	}
	if has, err := classify(exitWith(t, 44), ""); has || err != nil {
		t.Errorf("exit 44 = %v, %v; want absent, no error", has, err)
	}
	if has, err := classify(exitWith(t, 36), ""); has || !errors.Is(err, ErrLocked) {
		t.Errorf("exit 36 = %v, %v; want ErrLocked", has, err)
	}
	if has, err := classify(exitWith(t, 1), "security: The specified item could not be found in the keychain."); has || err != nil {
		t.Errorf("not-found text = %v, %v", has, err)
	}
	if has, err := classify(exitWith(t, 1), "User interaction is not allowed."); has || !errors.Is(err, ErrLocked) {
		t.Errorf("interaction text = %v, %v", has, err)
	}
	if has, err := classify(exitWith(t, 51), "security: something else"); has || err == nil || errors.Is(err, ErrLocked) {
		t.Errorf("other exit = %v, %v; want a plain error", has, err)
	}
	if _, err := classify(errors.New("not an exec error"), ""); err == nil {
		t.Error("a non-exec error should be returned")
	}
}
