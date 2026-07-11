// Package trail writes a best-effort shale evidence trail (intent → notes →
// done) for every mason task. Silent no-op when the shale binary is absent.
// Never receives credentials or file contents — only tool names and compact
// metadata.
package trail

import (
	"fmt"
	"os/exec"
)

type Trail struct {
	active bool
	root   string
}

func New(root, task string) *Trail {
	if _, err := exec.LookPath("shale"); err != nil {
		return &Trail{}
	}
	cmd := exec.Command("shale", "intent", task)
	cmd.Dir = root
	if err := cmd.Run(); err != nil {
		return &Trail{}
	}
	return &Trail{active: true, root: root}
}

func (t *Trail) Note(format string, args ...any) {
	if !t.active {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	cmd := exec.Command("shale", "note", msg)
	cmd.Dir = t.root
	_ = cmd.Run()
}

func (t *Trail) Done() {
	if !t.active {
		return
	}
	cmd := exec.Command("shale", "done")
	cmd.Dir = t.root
	_ = cmd.Run()
}
