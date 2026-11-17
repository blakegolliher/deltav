package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Styles use the terminal's own 16 colors and attributes, so the UI follows
// the terminal's theme and reads the same on light and dark backgrounds.
var (
	styleTitle     = lipgloss.NewStyle().Bold(true)
	styleTab       = lipgloss.NewStyle().Faint(true).Padding(0, 1)
	styleTabActive = lipgloss.NewStyle().Bold(true).Reverse(true).Padding(0, 1)
	styleHead      = lipgloss.NewStyle().Bold(true)
	styleDim       = lipgloss.NewStyle().Faint(true)
	styleGood      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleBad       = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	styleSelected  = lipgloss.NewStyle().Reverse(true)
)

// rate formats a per-second rate: "0", "0.4", "12.3", "1.2k", "3.4M".
func rate(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v < 10:
		return fmt.Sprintf("%.1f", v)
	case v < 1000:
		return fmt.Sprintf("%.0f", v)
	case v < 1e6:
		return fmt.Sprintf("%.1fk", v/1e3)
	}
	return fmt.Sprintf("%.1fM", v/1e6)
}

// mbps formats a byte rate in megabytes per second.
func mbps(bytesPerSecond float64) string {
	mb := bytesPerSecond / 1e6
	switch {
	case mb == 0:
		return "0"
	case mb < 0.05:
		return "<0.1"
	case mb < 100:
		return fmt.Sprintf("%.1f", mb)
	}
	return fmt.Sprintf("%.0f", mb)
}

// millis formats a latency given in milliseconds: "-" when there is none.
func millis(ms float64) string {
	switch {
	case ms <= 0:
		return "-"
	case ms < 10:
		return fmt.Sprintf("%.1fms", ms)
	case ms < 1000:
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.1fs", ms/1000)
}

// age formats how long ago t was.
func age(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// sparkline draws the last width points of history, scaled to their own
// maximum. A flat zero history is drawn as dots, so "no traffic" does not
// look like "low traffic".
func sparkline(history []float64, width int) string {
	if width <= 0 {
		return ""
	}
	if len(history) > width {
		history = history[len(history)-width:]
	}
	peak := 0.0
	for _, v := range history {
		peak = max(peak, v)
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", width-len(history)))
	for _, v := range history {
		switch {
		case peak == 0 || v == 0:
			b.WriteRune('·')
		default:
			i := int(v / peak * float64(len(sparkRunes)-1))
			b.WriteRune(sparkRunes[i])
		}
	}
	return b.String()
}

// column is one table column.
type column struct {
	title string
	right bool // right-aligned, for numbers
}

// table lays rows out in aligned columns under a header and returns the
// lines, each cut to width. selected is the index of the highlighted row,
// or -1. Cells may carry styling; alignment uses their visible width.
func table(cols []column, rows [][]string, selected, width int) []string {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = lipgloss.Width(c.title)
	}
	for _, r := range rows {
		for i := range cols {
			if i < len(r) {
				widths[i] = max(widths[i], lipgloss.Width(r[i]))
			}
		}
	}
	line := func(cells []string, style func(string) string) string {
		var b strings.Builder
		for i, c := range cols {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			pad := strings.Repeat(" ", widths[i]-lipgloss.Width(cell))
			if i > 0 {
				b.WriteString("  ")
			}
			if c.right {
				b.WriteString(pad + style(cell))
			} else {
				b.WriteString(style(cell) + pad)
			}
		}
		return ansi.Truncate(b.String(), width, "…")
	}
	titles := make([]string, len(cols))
	for i, c := range cols {
		titles[i] = c.title
	}
	out := []string{line(titles, func(s string) string { return styleHead.Render(s) })}
	for i, r := range rows {
		if i == selected {
			// Strip the cells' own colors: reverse video over them is
			// hard to read.
			plain := make([]string, len(r))
			for j, c := range r {
				plain[j] = ansi.Strip(c)
			}
			out = append(out, styleSelected.Render(ansi.Strip(line(plain, func(s string) string { return s }))))
			continue
		}
		out = append(out, line(r, func(s string) string { return s }))
	}
	return out
}

// errorShare colors a count of failures: dim when zero.
func failures(perSecond float64, n uint64) string {
	if n == 0 {
		return styleDim.Render("0")
	}
	return styleWarn.Render(rate(perSecond))
}
