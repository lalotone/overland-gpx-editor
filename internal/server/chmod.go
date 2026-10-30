package server

import "os"

// chmodIn sets the mode of name inside root without os.Root.Chmod.
//
// Root.Chmod asks the kernel not to follow a final symlink, which on Linux
// needs the fchmodat2 syscall. Android's seccomp policy does not allow it on
// every release, and seccomp does not fail the call: it kills the app. Opening
// the name through the root keeps the same confinement (the open cannot leave
// the root) and fchmod on the descriptor works on every kernel.
func chmodIn(root *os.Root, name string, mode os.FileMode) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(mode)
}
