//go:build windows

package agent

import "os/exec"

// configureBashCancel: Windows has no POSIX process groups; the default
// CommandContext kill plus WaitDelay (set by the caller) bounds the wait.
func configureBashCancel(_ *exec.Cmd) {}
