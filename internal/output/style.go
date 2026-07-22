// Package output provides terminal styling that degrades cleanly when
// stdout isn't a TTY (piped, redirected, or NO_COLOR is set), rather than
// emitting raw ANSI escapes into logs and files.
package output

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

var colorEnabled = isTerminal(os.Stdout.Fd()) && os.Getenv("NO_COLOR") == ""

// isTerminal reports whether fd refers to a TTY, via the same TCGETS
// ioctl approach golang.org/x/term uses on Linux, without adding a new
// module dependency for it — internal/fs/clone.go already scopes this
// codebase's syscall use to Linux via unix.IoctlFileClone.
func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	return err == nil
}

const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[2m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiCyan    = "\033[36m"
	ansiMagenta = "\033[35m"
)

func style(code, s string) string {
	if !colorEnabled {
		return s
	}
	return code + s + ansiReset
}

// Bold highlights text that needs emphasis without implying a status,
// such as a command the user should run next.
func Bold(s string) string { return style(ansiBold, s) }

// Dim de-emphasizes secondary detail, such as an already-known default.
func Dim(s string) string { return style(ansiDim, s) }

// Warn marks text describing a skipped step or a condition worth the
// user's attention, short of an outright error.
func Warn(s string) string { return style(ansiYellow, s) }

// Success marks a confirmation that something completed as expected.
func Success(s string) string { return style(ansiGreen, s) }

// Command highlights a literal command the user can copy and run.
func Command(s string) string { return style(ansiCyan, s) }

// Commandf is Command with fmt.Sprintf-style formatting applied first.
func Commandf(format string, args ...any) string {
	return Command(fmt.Sprintf(format, args...))
}

// Name highlights a worktree's name — Grove's stable identifier for it
// (see resolveWorktreeName in cmd/create.go) — distinctly from a bare
// filesystem path or branch, so the three don't blur together in
// output that shows all three at once.
func Name(s string) string { return style(ansiMagenta, s) }

// Path highlights a filesystem path.
func Path(s string) string { return style(ansiBold, s) }
