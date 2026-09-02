//go:build !darwin

package accounts

import "errors"

// MacKeychain does not exist off macOS; every call says so, and nodes use
// env: refs.
type MacKeychain struct{}

var ErrLocked = errors.New("no keychain on this platform; use an env: ref")

func (MacKeychain) Has(string) (bool, error)   { return false, ErrLocked }
func (MacKeychain) Get(string) (string, error) { return "", ErrLocked }
func (MacKeychain) Set(string) error           { return ErrLocked }
func SetArgs(service string) []string          { return nil }
