package webui

import (
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"
)

// Formatting for the pages. Numbers are short enough to scan in a table
// column and carry no unit: the column header or the label beside a figure
// names it.

// count formats a whole number with thousands separators: "12,480".
func count(v any) string {
	var s string
	switch n := v.(type) {
	case int:
		s = strconv.Itoa(n)
	case int64:
		s = strconv.FormatInt(n, 10)
	case uint32:
		s = strconv.FormatUint(uint64(n), 10)
	case uint64:
		s = strconv.FormatUint(n, 10)
	default:
		return fmt.Sprint(v)
	}
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return sign + b.String()
}

// rate formats a per-second rate: "0", "0.4", "12", "1.2K", "3.4M".
func rate(v float64) string {
	switch {
	case v <= 0:
		return "0"
	case v < 0.05:
		return "<0.1"
	case v < 10:
		return strconv.FormatFloat(v, 'f', 1, 64)
	case v < 1000:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case v < 1e6:
		return strconv.FormatFloat(v/1e3, 'f', 1, 64) + "K"
	}
	return strconv.FormatFloat(v/1e6, 'f', 1, 64) + "M"
}

// mbps formats a byte rate in megabytes (10^6 bytes) per second.
func mbps(bytesPerSecond float64) string {
	mb := bytesPerSecond / 1e6
	switch {
	case mb <= 0:
		return "0"
	case mb < 0.05:
		return "<0.1"
	case mb < 100:
		return strconv.FormatFloat(mb, 'f', 1, 64)
	}
	return count(int64(math.Round(mb)))
}

// millis formats a latency given in milliseconds; "–" when there is none.
func millis(ms float64) string {
	switch {
	case ms <= 0:
		return "–"
	case ms < 10:
		return strconv.FormatFloat(ms, 'f', 1, 64) + " ms"
	case ms < 1000:
		return strconv.FormatFloat(ms, 'f', 0, 64) + " ms"
	}
	return strconv.FormatFloat(ms/1000, 'f', 1, 64) + " s"
}

// share formats part of whole as a percentage: "0", "0.4%", "12%"; "–"
// when there is no whole to take a share of.
func share(part, whole uint64) string {
	switch {
	case whole == 0:
		return "–"
	case part == 0:
		return "0"
	}
	p := float64(part) / float64(whole) * 100
	switch {
	case p < 0.05:
		return "<0.1%"
	case p < 10:
		return strconv.FormatFloat(p, 'f', 1, 64) + "%"
	}
	return strconv.FormatFloat(p, 'f', 0, 64) + "%"
}

// ago formats how long before now t was: "12s ago", "never" for no time.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// Sparkline geometry, in CSS pixels. The drawing is not stretched, so the
// line keeps its weight and the end dot stays round.
const (
	sparkW, sparkH = 120.0, 28.0
	sparkPad       = 4.0 // room for the end dot and its ring
)

// sparkline draws a request-rate history as an inline SVG: a quiet line
// scaled to its own peak, with the latest value marked. A history with no
// traffic is a flat baseline with no mark, so "none" does not look like
// "low". what names the series for the accessible label. Only numbers and
// what reach the markup, and what is escaped.
func sparkline(history []float64, what string) template.HTML {
	peak, low, last := 0.0, math.Inf(1), 0.0
	for _, v := range history {
		peak, low = math.Max(peak, v), math.Min(low, v)
	}
	if len(history) > 0 {
		last = history[len(history)-1]
	} else {
		low = 0
	}
	label := fmt.Sprintf("%s, requests per second over the last %s: low %s, peak %s, latest %s",
		what, span(len(history)), rate(low), rate(peak), rate(last))
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" width="%g" height="%g" viewBox="0 0 %g %g" role="img" aria-label="%s"><title>%s</title>`,
		sparkW, sparkH, sparkW, sparkH, template.HTMLEscapeString(label), template.HTMLEscapeString(label))
	base := sparkH - sparkPad
	if peak <= 0 || len(history) < 2 {
		fmt.Fprintf(&b, `<line class="spark-flat" x1="%g" y1="%g" x2="%g" y2="%g"/></svg>`, sparkPad, base, sparkW-sparkPad, base)
		return template.HTML(b.String()) //nolint:gosec // numbers and an escaped label only
	}
	step := (sparkW - 2*sparkPad) / float64(len(history)-1)
	pts := make([]string, len(history))
	var x, y float64
	for i, v := range history {
		x = sparkPad + float64(i)*step
		y = base - v/peak*(sparkH-2*sparkPad)
		pts[i] = num(x) + "," + num(y)
	}
	line := strings.Join(pts, " ")
	fmt.Fprintf(&b, `<polygon class="spark-area" points="%s,%s %s %s,%s"/>`, num(sparkPad), num(base), line, num(x), num(base))
	fmt.Fprintf(&b, `<polyline class="spark-line" points="%s"/>`, line)
	fmt.Fprintf(&b, `<circle class="spark-now" cx="%s" cy="%s" r="3"/></svg>`, num(x), num(y))
	return template.HTML(b.String()) //nolint:gosec // numbers and an escaped label only
}

// span says how much time n ten-second slots cover: "5 minutes".
func span(slots int) string {
	secs := slots * 10
	if secs >= 90 {
		return fmt.Sprintf("%d minutes", int(math.Round(float64(secs)/60)))
	}
	return fmt.Sprintf("%d seconds", secs)
}

func num(v float64) string { return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) }

// Meter geometry, in CSS pixels.
const meterW, meterH = 96.0, 6.0

// meter draws used against limit as a thin bar. The fill carries the
// severity: within the limit, close to it (from 80%), over it. Its track is
// a lighter step of the same color, so the state reads along the whole bar.
// The figures themselves are printed beside it by the caller.
func meter(used uint64, limit uint32) template.HTML {
	level, frac := "ok", 0.0
	if limit > 0 {
		frac = float64(used) / float64(limit)
	}
	switch {
	case limit > 0 && used > uint64(limit):
		level, frac = "over", 1
	case frac >= 0.8:
		level = "near"
	}
	w := math.Round(frac*meterW*10) / 10
	if used > 0 && w < meterH {
		w = meterH // a sliver would not show as a rounded bar
	}
	label := fmt.Sprintf("%s of %s", count(used), count(limit))
	return template.HTML(fmt.Sprintf( //nolint:gosec // numbers only
		`<svg class="meter meter-%s" width="%g" height="%g" viewBox="0 0 %g %g" role="img" aria-label="%s">`+
			`<rect class="meter-track" width="%g" height="%g" rx="%g"/><rect class="meter-fill" width="%s" height="%g" rx="%g"/></svg>`,
		level, meterW, meterH, meterW, meterH, label, meterW, meterH, meterH/2, num(w), meterH, meterH/2))
}
