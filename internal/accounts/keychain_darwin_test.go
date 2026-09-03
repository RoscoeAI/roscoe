//go:build darwin

package accounts

import "testing"

// security only prompts for the secret when -w is the final argument with no
// value. Anywhere else, or absent, it stores an empty password silently,
// which is how a real token once failed to land.
func TestSetArgsPromptsForTheSecret(t *testing.T) {
	args := SetArgs("roscoe-account-primary")
	if args[len(args)-1] != "-w" {
		t.Fatalf("last argument is %q, want -w so security prompts: %v", args[len(args)-1], args)
	}
	for i, a := range args[:len(args)-1] {
		if a == "-w" {
			t.Fatalf("-w at position %d would take the next argument as the password: %v", i, args)
		}
	}
	want := map[string]bool{"-U": false, "-s": false, "-a": false}
	for _, a := range args {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for flag, seen := range want {
		if !seen {
			t.Errorf("missing %s in %v", flag, args)
		}
	}
}
