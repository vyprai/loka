package logs

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Range presets cycled with Tab.
var logRangePresets = []string{"5m", "15m", "1h", "6h", "24h"}

// --- Bubbletea messages ---

type logQueryResultMsg struct {
	entries []logLine
	elapsed time.Duration
	err     error
}

// logLine is a single log entry for display.
type logLine struct {
	timestamp string
	level     string
	labels    string
	message   string
}

// --- Model ---

// Model is the Bubbletea model for the log viewer TUI.
type Model struct {
	client   *LogsClient
	input    textinput.Model
	rangeIdx int
	entries  []logLine
	cursor   int
	offset   int // scroll offset for viewport
	width    int
	height   int
	elapsed  time.Duration
	err      error
	loading  bool
}

// NewModel creates a new TUI model.
func NewModel(client *LogsClient) Model {
	ti := textinput.New()
	ti.Placeholder = "Enter LogQL query... e.g. {type=\"service\"} |= \"error\""
	ti.Focus()
	ti.CharLimit = 256
	ti.Width = 60

	return Model{
		client:   client,
		input:    ti,
		rangeIdx: 2, // default 1h
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return textinput.Blink
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if !m.input.Focused() || msg.String() == "ctrl+c" {
				return m, tea.Quit
			}
		case "enter":
			query := strings.TrimSpace(m.input.Value())
			if query == "" {
				return m, nil
			}
			m.loading = true
			m.err = nil
			return m, m.executeQuery(query)
		case "tab":
			m.rangeIdx = (m.rangeIdx + 1) % len(logRangePresets)
			return m, nil
		case "up":
			if m.cursor > 0 {
				m.cursor--
				if m.cursor < m.offset {
					m.offset = m.cursor
				}
			}
			return m, nil
		case "down":
			if m.cursor < len(m.entries)-1 {
				m.cursor++
				maxVisible := m.viewportHeight()
				if m.cursor >= m.offset+maxVisible {
					m.offset = m.cursor - maxVisible + 1
				}
			}
			return m, nil
		case "pgup":
			m.cursor -= m.viewportHeight()
			if m.cursor < 0 {
				m.cursor = 0
			}
			m.offset = m.cursor
			return m, nil
		case "pgdown":
			m.cursor += m.viewportHeight()
			if m.cursor >= len(m.entries) {
				m.cursor = len(m.entries) - 1
			}
			if m.cursor < 0 {
				m.cursor = 0
			}
			maxVisible := m.viewportHeight()
			if m.cursor >= m.offset+maxVisible {
				m.offset = m.cursor - maxVisible + 1
			}
			return m, nil
		}

	case logQueryResultMsg:
		m.loading = false
		m.elapsed = msg.elapsed
		if msg.err != nil {
			m.err = msg.err
			m.entries = nil
		} else {
			m.entries = msg.entries
			m.err = nil
			m.cursor = 0
			m.offset = 0
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// viewportHeight returns how many log lines fit in the viewport.
func (m Model) viewportHeight() int {
	// title(1) + query(1) + range(1) + separator(1) + status(1) = 5 lines overhead
	h := m.height - 5
	if h < 1 {
		h = 20
	}
	return h
}

// View implements tea.Model.
func (m Model) View() string {
	var b strings.Builder

	width := m.width
	if width < 80 {
		width = 80
	}

	// Title bar.
	title := logTitleStyle.Render("loka logs")
	quitHint := lipgloss.NewStyle().
		Foreground(lipgloss.Color("243")).
		Background(lipgloss.Color("62")).
		Render("[q] quit")
	titlePad := width - lipgloss.Width(title) - lipgloss.Width(quitHint)
	if titlePad < 0 {
		titlePad = 0
	}
	titleBar := logTitleBarStyle.Width(width).Render(
		title + strings.Repeat(" ", titlePad) + quitHint,
	)
	b.WriteString(titleBar + "\n")

	// Query input row.
	b.WriteString(logLabelStyle.Render("  Query: "))
	b.WriteString(m.input.View())
	b.WriteString("  ")
	b.WriteString(logHelpStyle.Render("[Enter: run]"))
	b.WriteString("\n")

	// Range row.
	b.WriteString(logLabelStyle.Render("  Range: "))
	for i, r := range logRangePresets {
		if i == m.rangeIdx {
			b.WriteString(logActiveRangeStyle.Render("[" + r + "]"))
		} else {
			b.WriteString(logInactiveRangeStyle.Render(" " + r + " "))
		}
		b.WriteString(" ")
	}
	b.WriteString("\n")

	// Separator.
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(strings.Repeat("-", width)) + "\n")

	// Loading indicator.
	if m.loading {
		b.WriteString("  Querying...\n")
	}

	// Error.
	if m.err != nil {
		b.WriteString("  " + logErrorStyle.Render("Error: "+m.err.Error()) + "\n")
	}

	// Log entries.
	if len(m.entries) > 0 {
		maxVisible := m.viewportHeight()
		end := m.offset + maxVisible
		if end > len(m.entries) {
			end = len(m.entries)
		}

		for i := m.offset; i < end; i++ {
			e := m.entries[i]
			lvl := levelStyle(e.level).Render(fmt.Sprintf("%-5s", e.level))
			ts := timestampStyle.Render(e.timestamp)
			lbls := logLabelsStyle.Render(e.labels)
			msg := messageStyle.Render(e.message)

			// Truncate message to fit width.
			overhead := len(e.timestamp) + 1 + 5 + 1 + len(e.labels) + 1
			maxMsg := width - overhead - 2
			if maxMsg > 0 && len(e.message) > maxMsg {
				msg = messageStyle.Render(e.message[:maxMsg] + "...")
			}

			row := fmt.Sprintf("  %s %s %s %s", ts, lvl, lbls, msg)

			if i == m.cursor {
				row = logSelectedRowStyle.Width(width).Render(row)
			}
			b.WriteString(row + "\n")
		}
	}

	// Status bar.
	entryCount := len(m.entries)
	status := fmt.Sprintf("  %d entries | %s", entryCount, m.elapsed.Truncate(time.Millisecond))
	nav := "[Tab] range  [Up/Down] scroll  [PgUp/PgDn] page"
	statusPad := width - lipgloss.Width(status) - lipgloss.Width(nav)
	if statusPad < 0 {
		statusPad = 0
	}
	statusLine := logStatusBarStyle.Width(width).Render(
		status + strings.Repeat(" ", statusPad) + logHelpStyle.Render(nav),
	)
	b.WriteString(statusLine + "\n")

	return b.String()
}

// executeQuery returns a tea.Cmd that queries the logs API.
func (m Model) executeQuery(query string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		start := time.Now()

		rangeDur := logRangePresets[m.rangeIdx]
		now := time.Now()
		endTS := fmt.Sprintf("%d", now.UnixNano())
		startTS := fmt.Sprintf("%d", now.Add(-parseDuration(rangeDur)).UnixNano())

		resp, err := m.client.QueryRange(ctx, query, startTS, endTS, 1000)
		elapsed := time.Since(start)
		if err != nil {
			return logQueryResultMsg{err: err, elapsed: elapsed}
		}

		lines, err := parseStreamResults(resp)
		if err != nil {
			return logQueryResultMsg{err: err, elapsed: elapsed}
		}

		return logQueryResultMsg{entries: lines, elapsed: elapsed}
	}
}

// parseDuration converts a simple duration string to time.Duration.
func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Hour
	}
	return d
}

// parseStreamResults decodes a streams result into display lines.
func parseStreamResults(resp *QueryResponse) ([]logLine, error) {
	if resp.Data.ResultType != "streams" {
		return nil, fmt.Errorf("expected streams, got %s", resp.Data.ResultType)
	}

	var results []StreamResult
	if err := json.Unmarshal(resp.Data.Result, &results); err != nil {
		return nil, err
	}

	var lines []logLine
	for _, r := range results {
		lbls := formatLogLabels(r.Stream)
		for _, v := range r.Values {
			tsNs := v[0]
			msg := v[1]

			// Parse nanosecond timestamp.
			var ts time.Time
			if n, err := fmt.Sscanf(tsNs, "%d", new(int64)); err == nil && n == 1 {
				var nsec int64
				fmt.Sscanf(tsNs, "%d", &nsec)
				ts = time.Unix(0, nsec)
			}

			level := ""
			if l, ok := r.Stream["level"]; ok {
				level = strings.ToUpper(l)
			}

			lines = append(lines, logLine{
				timestamp: ts.Format("15:04:05.000"),
				level:     level,
				labels:    lbls,
				message:   msg,
			})
		}
	}

	// Sort by timestamp ascending (forward order for display).
	sort.Slice(lines, func(i, j int) bool {
		return lines[i].timestamp < lines[j].timestamp
	})

	return lines, nil
}

// formatLogLabels formats stream labels for display.
func formatLogLabels(m map[string]string) string {
	parts := make([]string, 0, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == "level" {
			continue // already shown as colored level badge
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	if len(parts) > 3 {
		return strings.Join(parts[:3], " ") + "..."
	}
	return strings.Join(parts, " ")
}
