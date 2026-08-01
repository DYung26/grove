package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/dyung/grove/internal/config"
	"github.com/dyung/grove/internal/git"
	"github.com/dyung/grove/internal/pool"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

var savingsCmd = &cobra.Command{
	Use:     "savings",
	Aliases: []string{"du"},
	Short:   "Show how much disk space Grove's CoW cloning is actually saving, per worktree",
	RunE:    runSavings,
}

func init() {
	rootCmd.AddCommand(savingsCmd)
}

// worktreeSavings is what runSavings needs to print one line of the
// summary: the worktree's tracked clone mode alongside the measured
// space its dependency dirs are using, apparent vs. actual.
type worktreeSavings struct {
	Name      string
	CloneMode string
	Usage     pool.DirUsage
}

func runSavings(cmd *cobra.Command, args []string) error {
	repoRoot, err := git.MainRepoRoot()
	if err != nil {
		return err
	}

	reg, err := config.Load(repoRoot)
	if err != nil {
		return err
	}

	if len(reg.Worktrees) == 0 {
		fmt.Println("No worktrees tracked yet.")
		return nil
	}

	dirs, err := resolveRepoDependencyDirs(repoRoot)
	if err != nil {
		return err
	}

	mainUsage, err := sumDirUsage(repoRoot, dirs)
	if err != nil {
		return err
	}

	results, err := collectWorktreeSavings(reg, dirs)
	if err != nil {
		return err
	}

	printSavingsReport(repoRoot, mainUsage, results)
	return nil
}

// collectWorktreeSavings measures dependency-dir usage for every tracked
// worktree, skipping any whose directory is missing on disk (surfaced
// separately by `grove status`, not a reason to fail this report).
func collectWorktreeSavings(reg *config.Registry, dirs []string) ([]worktreeSavings, error) {
	var results []worktreeSavings
	for _, wt := range reg.Worktrees {
		if _, err := os.Stat(wt.Path); err != nil {
			continue
		}

		usage, err := sumDirUsage(wt.Path, dirs)
		if err != nil {
			return nil, fmt.Errorf("measure %s: %w", wt.Name, err)
		}

		results = append(results, worktreeSavings{
			Name:      wt.Name,
			CloneMode: displayCloneMode(wt.DepsCloneMode),
			Usage:     usage,
		})
	}
	return results, nil
}

// resolveRepoDependencyDirs returns the flat list of repo-root-relative
// dependency dir paths, discarding the per-dir project ownership that
// resolveAllDependencyDirs also carries — savings.go and
// ensureDepsSourcePoolResident only ever need the paths, not which
// project each belongs to.
func resolveRepoDependencyDirs(repoRoot string) ([]string, error) {
	projectCfg, err := config.LoadProjectConfig(repoRoot)
	if err != nil {
		return nil, err
	}

	resolved, err := resolveAllDependencyDirs(repoRoot, projectCfg)
	if err != nil {
		return nil, err
	}

	dirs := make([]string, len(resolved))
	for i, r := range resolved {
		dirs[i] = r.Path
	}
	return dirs, nil
}

// sumDirUsage measures each of dirs under root and adds them together,
// skipping any that don't exist there (a worktree isn't guaranteed to
// have every dependency dir the repo config lists — e.g. one added after
// the worktree was created).
func sumDirUsage(root string, dirs []string) (pool.DirUsage, error) {
	var total pool.DirUsage
	for _, dir := range dirs {
		path := filepath.Join(root, dir)
		if _, err := os.Stat(path); err != nil {
			continue
		}

		usage, err := pool.MeasureDirUsage(path)
		if err != nil {
			return pool.DirUsage{}, err
		}
		total.Apparent += usage.Apparent
		total.Exclusive += usage.Exclusive
	}
	return total, nil
}

func displayCloneMode(mode string) string {
	switch mode {
	case "":
		return "unknown"
	case config.DepsCloneModeNone:
		return "n/a (nothing to clone)"
	default:
		return mode
	}
}

func printSavingsReport(repoRoot string, mainUsage pool.DirUsage, results []worktreeSavings) {
	fmt.Printf("main\t%s\t%s\n", repoRoot, formatBytes(mainUsage.Apparent))

	if len(results) == 0 {
		fmt.Println("\nNo tracked worktrees have measurable dependency dirs yet.")
		return
	}

	fmt.Println()
	printWorktreeTable(results)

	var totalApparent, totalExclusive uint64
	for _, r := range results {
		totalApparent += r.Usage.Apparent
		totalExclusive += r.Usage.Exclusive
	}

	saved := uint64(0)
	if totalApparent > totalExclusive {
		saved = totalApparent - totalExclusive
	}
	fmt.Printf("\nTotal: %s on disk across %d worktree(s); would be %s without CoW — saved %s\n",
		formatBytes(totalExclusive), len(results), formatBytes(totalApparent), formatBytes(saved))
}

// Column-width tuning for printWorktreeTable's worktree-name column: it's
// the only column whose content length varies wildly (branch names), so
// it's the only one that ever needs wrapping. minNameWidth keeps very
// narrow terminals usable; maxNameWidth keeps a wide terminal from
// stretching the name column further than it needs to just because room
// is available.
const (
	minNameWidth = 12
	maxNameWidth = 40
	tabwriterPadding = 2
)

// printWorktreeTable renders the per-worktree savings table with
// text/tabwriter so columns stay aligned regardless of value width. The
// worktree-name column is capped and wrapped onto extra lines instead of
// letting a long branch name (e.g. "feat/sparkler-tags") push every
// later column out of alignment or off the edge of the terminal. The
// other columns (mode, formatted byte sizes) hold short, bounded values
// that never realistically need wrapping.
func printWorktreeTable(results []worktreeSavings) {
	header := []string{"worktree", "mode", "size on disk", "if not CoW'd", "saved"}
	rows := make([][]string, len(results))
	for i, r := range results {
		rows[i] = []string{
			r.Name,
			r.CloneMode,
			formatBytes(r.Usage.Exclusive),
			formatBytes(r.Usage.Apparent),
			formatBytes(r.Usage.Saved()),
		}
	}

	nameWidth := nameColumnWidth(header, rows)

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, tabwriterPadding, ' ', 0)
	writeWrappedRow(tw, header, nameWidth)
	for _, row := range rows {
		writeWrappedRow(tw, row, nameWidth)
	}
	tw.Flush()
}

// nameColumnWidth picks how wide the worktree-name column is allowed to
// get before wrapping, based on how much room is left on the terminal
// after the other columns (whose width is just their longest actual
// value plus tabwriter's own padding). Returns 0 (meaning "don't wrap")
// when output isn't going to an interactive terminal (e.g. piped to a
// file or into `less`), matching how most CLI tools skip wrapping for
// non-interactive output.
func nameColumnWidth(header []string, rows [][]string) int {
	width := terminalWidth()
	if width <= 0 {
		return 0
	}

	otherCols := 0
	for col := 1; col < len(header); col++ {
		w := len(header[col])
		for _, row := range rows {
			if len(row[col]) > w {
				w = len(row[col])
			}
		}
		otherCols += w + tabwriterPadding
	}

	available := width - otherCols
	switch {
	case available < minNameWidth:
		return minNameWidth
	case available > maxNameWidth:
		return maxNameWidth
	default:
		return available
	}
}

// writeWrappedRow writes row to tw, wrapping row[0] (the worktree name)
// onto additional lines if it's wider than nameWidth. Continuation lines
// leave every other column blank so tabwriter still sees the same number
// of cells on every line and keeps the later columns aligned.
func writeWrappedRow(tw *tabwriter.Writer, row []string, nameWidth int) {
	for i, line := range wrapCell(row[0], nameWidth) {
		if i == 0 {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", line, row[1], row[2], row[3], row[4])
		} else {
			fmt.Fprintf(tw, "%s\t\t\t\t\n", line)
		}
	}
}

// wrapCell splits s into lines of at most width runes. width <= 0 means
// "don't wrap" (used when the terminal size couldn't be determined).
// Breaks are preferred at '/' (branch names are often "feat/foo") so a
// wrapped name stays readable; if a single segment between slashes is
// still longer than width, it's hard-broken instead.
func wrapCell(s string, width int) []string {
	if width <= 0 || len(s) <= width {
		return []string{s}
	}

	var lines []string
	for len(s) > width {
		breakAt := strings.LastIndex(s[:width+1], "/")
		if breakAt <= 0 {
			breakAt = width
		}
		lines = append(lines, s[:breakAt])
		s = strings.TrimPrefix(s[breakAt:], "/")
	}
	if s != "" {
		lines = append(lines, s)
	}
	return lines
}

// terminalWidth reports the current terminal's column count, or 0 if
// stdout isn't a terminal (piped output, redirected to a file, etc.) or
// the ioctl otherwise fails.
func terminalWidth() int {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(ws.Col)
}

func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}

	div, exp := uint64(unit), 0
	for next := n / unit; next >= unit; next /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
