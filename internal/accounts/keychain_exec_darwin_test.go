//go:build darwin

package accounts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeSecurity puts a stand-in "security" first on PATH. It keeps one file
// per service, reads the secret from stdin only when -w is the last argument
// (as the real tool does), and stores an empty secret otherwise.
func fakeSecurity(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	store := filepath.Join(dir, "store")
	os.MkdirAll(store, 0o755)
	script := `#!/bin/sh
sub="$1"; shift
svc=""; last=""
for a in "$@"; do [ "$last" = "-s" ] && svc="$a"; last="$a"; done
case "$sub" in
  find-generic-password)
    [ -f "$STORE/$svc" ] || exit 44
    case " $* " in *" -w "*) cat "$STORE/$svc";; esac; exit 0;;
  add-generic-password)
    if [ "$last" = "-w" ]; then IFS= read -r s; printf '%s' "$s" > "$STORE/$svc"; else : > "$STORE/$svc"; fi; exit 0;;
  delete-generic-password) rm -f "$STORE/$svc"; exit 0;;
esac
exit 1
`
	if err := os.WriteFile(filepath.Join(dir, "security"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STORE", store)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return store
}

// Set must leave a token behind when one is pasted, and must not leave an
// empty item behind when nothing is.
func TestSetStoresWhatWasPasted(t *testing.T) {
	store := fakeSecurity(t)
	r, w, _ := os.Pipe()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	go func() { w.Write([]byte("sk-ant-oat01-test\n")); w.Close() }()

	kc := MacKeychain{}
	if err := kc.Set("roscoe-account-primary"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := kc.Get("roscoe-account-primary")
	if err != nil || got != "sk-ant-oat01-test" {
		t.Fatalf("Get after Set = %q, %v", got, err)
	}
	if has, _ := kc.Has("roscoe-account-primary"); !has {
		t.Error("Has says the item is missing")
	}
	if _, err := os.Stat(filepath.Join(store, "roscoe-account-primary")); err != nil {
		t.Error("the fake store has no item")
	}
}

func TestSetRejectsAnEmptyPaste(t *testing.T) {
	store := fakeSecurity(t)
	r, w, _ := os.Pipe()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	go func() { w.Write([]byte("\n")); w.Close() }()

	kc := MacKeychain{}
	err := kc.Set("roscoe-account-primary")
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("Set with an empty paste = %v, want ErrEmpty", err)
	}
	if _, err := os.Stat(filepath.Join(store, "roscoe-account-primary")); err == nil {
		t.Error("an empty item was left in the keychain, which reads as a stored token")
	}
	if has, _ := kc.Has("roscoe-account-primary"); has {
		t.Error("Has reports a token that is empty")
	}
}
