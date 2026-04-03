package lokametrics

import "github.com/charmbracelet/lipgloss"

// TUI styles.
var (
	// Title bar.
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("62")).
			Padding(0, 1)

	titleBarStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("62")).
			Width(80)

	// Input area.
	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243")).
			Bold(true)

	activeRangeStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("86"))

	inactiveRangeStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("243"))

	// Table header.
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("86")).
			BorderBottom(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240"))

	// Table rows.
	metricNameStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("75"))

	labelValueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243"))

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("220"))

	sparklineStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("35"))

	selectedRowStyle = lipgloss.NewStyle().
				Background(lipgloss.Color("237"))

	// Status bar.
	statusBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243")).
			BorderTop(true).
			BorderStyle(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("240"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Bold(true)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("243"))
)
