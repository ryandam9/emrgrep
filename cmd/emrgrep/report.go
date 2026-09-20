package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// mdMatch is one recorded match line for the -md report, already
// sanitized. Key is the full s3:// URI so multi-cluster runs stay
// unambiguous.
type mdMatch struct {
	Key    string
	Entry  string // ZIP entry name, "" otherwise
	LineNo int64
	Text   string
}

// reportPath is where -md writes: ~/logscan/<yyyy-mm-dd>/<app-id>.md,
// the date directory grouping reports by run day in local time (the
// same clock the report's Generated header shows).
func reportPath(appID string, now time.Time) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory for the -md report: %w", err)
	}
	return filepath.Join(home, "logscan", now.Format("2006-01-02"), appID+".md"), nil
}

// downloadDir is where -download stores files:
// ~/logscan/<yyyy-mm-dd>/<app-id>/ — a sibling of the -md report for
// the same application and day.
func downloadDir(appID string, now time.Time) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory for -download: %w", err)
	}
	return filepath.Join(home, "logscan", now.Format("2006-01-02"), appID), nil
}

// fenceFor returns a backtick fence long enough that content can never
// close it early: one backtick more than the longest run in the
// content, minimum the standard three.
func fenceFor(content string) string {
	longest, run := 0, 0
	for _, r := range content {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	return strings.Repeat("`", n)
}

// mdCodeBlock renders content as an sh-highlighted fenced code block.
func mdCodeBlock(content string) string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		content = "(none)"
	}
	fence := fenceFor(content)
	return fence + "sh\n" + content + "\n" + fence + "\n"
}

// writeMDReport renders and writes the -md Markdown report: the run's
// header facts, the matched file names, the matches grouped per file
// (each file a heading with its lines in one block), and the run
// summary.
func writeMDReport(path, appID, pattern string, scopes, matchedKeys []string, matches []mdMatch, runLog string, now time.Time) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# emrgrep — %s\n\n", appID)
	// Local time, zone spelled out: the report is read on the machine
	// that produced it, and "when did I run this" should not require
	// UTC arithmetic.
	fmt.Fprintf(&b, "- **Generated**: %s\n", now.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "- **Pattern**: `%s`\n", pattern)
	for _, s := range scopes {
		fmt.Fprintf(&b, "- **Scanned**: `%s`\n", s)
	}
	fmt.Fprintf(&b, "- **Files with matches**: %d\n\n", len(matchedKeys))

	b.WriteString("## Files with matches\n\n")
	b.WriteString(mdCodeBlock(strings.Join(matchedKeys, "\n")))

	b.WriteString("\n## Matches\n\n")
	if len(matches) == 0 {
		b.WriteString(mdCodeBlock(""))
		b.WriteString("\n")
	} else {
		// One section per file, in the order files first produced a
		// match; each file's lines keep their emission order.
		byKey := make(map[string][]mdMatch)
		var order []string
		for _, m := range matches {
			if _, seen := byKey[m.Key]; !seen {
				order = append(order, m.Key)
			}
			byKey[m.Key] = append(byKey[m.Key], m)
		}
		for _, key := range order {
			fmt.Fprintf(&b, "### %s\n\n", key)
			var lines strings.Builder
			for _, m := range byKey[key] {
				if m.Entry != "" {
					fmt.Fprintf(&lines, "%s:%d: %s\n", m.Entry, m.LineNo, m.Text)
				} else {
					fmt.Fprintf(&lines, "%6d: %s\n", m.LineNo, m.Text)
				}
			}
			b.WriteString(mdCodeBlock(lines.String()))
			b.WriteString("\n")
		}
	}

	b.WriteString("## Run summary\n\n")
	b.WriteString(mdCodeBlock(runLog))

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating report directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}
	return nil
}
