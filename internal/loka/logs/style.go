package logs

import "github.com/charmbracelet/lipgloss"

// TUI styles for the log viewer.
var (
	// Title bar.
	logTitleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("62")).
			Padding(0, 1)

	logTitleBarStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("62")).
				Width(80)

	// Input area.
	logLabelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243")).
			Bold(true)

	logActiveRangeStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("86"))

	logInactiveRangeStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("243"))

	// Log levels.
	levelDebugStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243")) // gray

	levelInfoStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("15")) // white

	levelWarnStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("220")) // yellow

	levelErrorStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("196")) // red

	// Timestamp.
	timestampStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243"))

	// Labels in log lines.
	logLabelsStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("75"))

	// Message text.
	messageStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	// Selected row.
	logSelectedRowStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("237"))

	// Status bar.
	logStatusBarStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("243")).
				BorderTop(true).
				BorderStyle(lipgloss.NormalBorder()).
				BorderForeground(lipgloss.Color("240"))

	logErrorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true)

	logHelpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243"))
)

// levelStyle returns the lipgloss style for a given log level.
func levelStyle(level string) lipgloss.Style {
	switch level {
	case "DEBUG", "debug":
		return levelDebugStyle
	case "WARN", "warn", "WARNING", "warning":
		return levelWarnStyle
	case "ERROR", "error", "FATAL", "fatal":
		return levelErrorStyle
	default:
		return levelInfoStyle
	}
}
