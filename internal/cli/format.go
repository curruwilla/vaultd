package cli

import (
	"fmt"
	"time"
)

// humanBytes renders a size the way an operator reads it in a report.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for size := n / unit; size >= unit && exp < 4; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

// humanDuration keeps a duration readable: seconds with one decimal for short
// runs, minutes and seconds for long ones.
func humanDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// ratio renders how much the pipeline shrank the dump.
func ratio(plaintext, stored int64) string {
	if stored <= 0 || plaintext <= 0 {
		return ""
	}
	return fmt.Sprintf("%.1f×", float64(plaintext)/float64(stored))
}

// shortSHA is enough of a checksum to compare by eye; the manifest holds all of it.
func shortSHA(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12] + "…"
}
