package agent

import (
	"fmt"
	"io"
	"strings"
)

// style is minimal ANSI coloring for the interactive terminal; disabled it
// passes text through untouched (pipes, CI, tests).
type style struct{ on bool }

func (st style) dim(s string) string    { return st.wrap("\x1b[2m", s) }
func (st style) cyan(s string) string   { return st.wrap("\x1b[36m", s) }
func (st style) green(s string) string  { return st.wrap("\x1b[32m", s) }
func (st style) red(s string) string    { return st.wrap("\x1b[31m", s) }
func (st style) yellow(s string) string { return st.wrap("\x1b[33m", s) }
func (st style) bold(s string) string   { return st.wrap("\x1b[1m", s) }

func (st style) wrap(code, s string) string {
	if !st.on {
		return s
	}
	return code + s + "\x1b[0m"
}

// prefixWriter indents every line it writes — used to nest subagent output
// under the parent's transcript.
type prefixWriter struct {
	w      io.Writer
	prefix string
	midLine bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	s := string(b)
	var out strings.Builder
	for _, line := range strings.SplitAfter(s, "\n") {
		if line == "" {
			continue
		}
		if !p.midLine {
			out.WriteString(p.prefix)
		}
		out.WriteString(line)
		p.midLine = !strings.HasSuffix(line, "\n")
	}
	if _, err := fmt.Fprint(p.w, out.String()); err != nil {
		return 0, err
	}
	return len(b), nil
}
