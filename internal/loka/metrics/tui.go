package lokametrics

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
var rangePresets = []string{"5m", "15m", "1h", "6h", "24h"}

// stepForRange picks a reasonable step for a given range.
var stepForRange = map[string]string{
	"5m":  "15s",
	"15m": "30s",
	"1h":  "1m",
	"6h":  "5m",
	"24h": "15m",
}

// --- Bubbletea messages ---

type queryResultMsg struct {
	series   []seriesRow
	elapsed  time.Duration
	err      error
}

// seriesRow is a single metric series with its display data.
type seriesRow struct {
	name      string
	labels    string
	lastValue string
	sparkline string
	values    []float64
}

// --- Model ---

// Model is the Bubbletea model for the metrics TUI.
type Model struct {
	client     *MetricsClient
	input      textinput.Model
	rangeIdx   int
	series     []seriesRow
	cursor     int
	width      int
	height     int
	elapsed    time.Duration
	err        error
	loading    bool
}

// NewModel creates a new TUI model.
func NewModel(client *MetricsClient) Model {
	ti := textinput.New()
	ti.Placeholder = "Enter PromQL query..."
	ti.Focus()
	ti.CharLimit = 256
	ti.Width = 50

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
			// If input is focused, 'q' is just a character.
		case "enter":
			query := strings.TrimSpace(m.input.Value())
			if query == "" {
				return m, nil
			}
			m.loading = true
			m.err = nil
			return m, m.executeQuery(query)
		case "tab":
			m.rangeIdx = (m.rangeIdx + 1) % len(rangePresets)
			return m, nil
		case "up":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "down":
			if m.cursor < len(m.series)-1 {
				m.cursor++
			}
			return m, nil
		}

	case queryResultMsg:
		m.loading = false
		m.elapsed = msg.elapsed
		if msg.err != nil {
			m.err = msg.err
			m.series = nil
		} else {
			m.series = msg.series
			m.err = nil
			m.cursor = 0
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

// View implements tea.Model.
func (m Model) View() string {
	var b strings.Builder

	// Title bar.
	width := m.width
	if width < 60 {
		width = 60
	}
	title := titleStyle.Render("loka metrics")
	quitHint := lipgloss.NewStyle().
		Foreground(lipgloss.Color("243")).
		Background(lipgloss.Color("62")).
		Render("[q] quit")
	titlePad := width - lipgloss.Width(title) - lipgloss.Width(quitHint)
	if titlePad < 0 {
		titlePad = 0
	}
	titleBar := titleBarStyle.Width(width).Render(
		title + strings.Repeat(" ", titlePad) + quitHint,
	)
	b.WriteString(titleBar + "\n")

	// Query input row.
	b.WriteString(labelStyle.Render("  Query: "))
	b.WriteString(m.input.View())
	b.WriteString("  ")
	b.WriteString(helpStyle.Render("[Enter: run]"))
	b.WriteString("\n")

	// Range / step row.
	b.WriteString(labelStyle.Render("  Range: "))
	for i, r := range rangePresets {
		if i == m.rangeIdx {
			b.WriteString(activeRangeStyle.Render("[" + r + "]"))
		} else {
			b.WriteString(inactiveRangeStyle.Render(" " + r + " "))
		}
		b.WriteString(" ")
	}
	step := stepForRange[rangePresets[m.rangeIdx]]
	b.WriteString(labelStyle.Render(" Step: "))
	b.WriteString(activeRangeStyle.Render(step))
	b.WriteString("\n")

	// Separator.
	b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(strings.Repeat("─", width)) + "\n")

	// Loading indicator.
	if m.loading {
		b.WriteString("  Querying...\n")
	}

	// Error.
	if m.err != nil {
		b.WriteString("  " + errorStyle.Render("Error: "+m.err.Error()) + "\n")
	}

	// Results table.
	if len(m.series) > 0 {
		// Header.
		hdr := fmt.Sprintf("  %-24s %-30s %s", "METRIC", "LABELS", "VALUE")
		b.WriteString(headerStyle.Width(width).Render(hdr) + "\n")

		sparkWidth := 20
		if width > 80 {
			sparkWidth = 30
		}

		for i, s := range m.series {
			row := fmt.Sprintf("  %-24s %-30s %s",
				metricNameStyle.Render(s.name),
				labelValueStyle.Render(s.labels),
				valueStyle.Render(s.lastValue),
			)
			sparkRow := "  " + strings.Repeat(" ", 24) + " " + sparklineStyle.Render(s.sparkline)

			// Re-render sparkline at current width if values are available.
			if len(s.values) > 0 {
				sparkRow = "  " + strings.Repeat(" ", 24) + " " +
					sparklineStyle.Render(RenderSparkline(s.values, sparkWidth))
			}

			if i == m.cursor {
				row = selectedRowStyle.Width(width).Render(row)
				sparkRow = selectedRowStyle.Width(width).Render(sparkRow)
			}

			b.WriteString(row + "\n")
			b.WriteString(sparkRow + "\n")
		}
	}

	// Status bar.
	seriesCount := len(m.series)
	status := fmt.Sprintf("  %d series | %s", seriesCount, m.elapsed.Truncate(time.Millisecond))
	nav := "[Tab] range  [Up/Down] navigate"
	statusPad := width - lipgloss.Width(status) - lipgloss.Width(nav)
	if statusPad < 0 {
		statusPad = 0
	}
	statusLine := statusBarStyle.Width(width).Render(
		status + strings.Repeat(" ", statusPad) + helpStyle.Render(nav),
	)
	b.WriteString(statusLine + "\n")

	return b.String()
}

// executeQuery returns a tea.Cmd that queries the metrics API.
func (m Model) executeQuery(query string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		start := time.Now()

		rangeDur := rangePresets[m.rangeIdx]
		step := stepForRange[rangeDur]

		now := time.Now()
		endTS := fmt.Sprintf("%d", now.Unix())
		startTS := fmt.Sprintf("%d", now.Add(-parseDuration(rangeDur)).Unix())

		resp, err := m.client.QueryRange(ctx, query, startTS, endTS, step)
		elapsed := time.Since(start)
		if err != nil {
			return queryResultMsg{err: err, elapsed: elapsed}
		}

		rows, err := parseMatrixResults(resp)
		if err != nil {
			// Fall back to instant query.
			instantResp, ierr := m.client.Query(ctx, query, nil)
			if ierr != nil {
				return queryResultMsg{err: err, elapsed: elapsed}
			}
			rows, err = parseVectorResults(instantResp)
			if err != nil {
				return queryResultMsg{err: err, elapsed: elapsed}
			}
		}

		return queryResultMsg{series: rows, elapsed: elapsed}
	}
}

// parseDuration converts a simple duration string (e.g. "5m", "1h", "24h") to time.Duration.
func parseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Hour // fallback
	}
	return d
}

// parseMatrixResults decodes a matrix (range) result into display rows.
func parseMatrixResults(resp *QueryResponse) ([]seriesRow, error) {
	if resp.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("expected matrix, got %s", resp.Data.ResultType)
	}

	var results []MatrixResult
	if err := json.Unmarshal(resp.Data.Result, &results); err != nil {
		return nil, err
	}

	rows := make([]seriesRow, 0, len(results))
	for _, r := range results {
		name := r.Metric["__name__"]
		labels := formatLabels(r.Metric)

		vals := make([]float64, len(r.Values))
		for i, v := range r.Values {
			vals[i] = v.Float()
		}

		lastVal := ""
		if len(vals) > 0 {
			lastVal = fmt.Sprintf("%.4g", vals[len(vals)-1])
		}

		rows = append(rows, seriesRow{
			name:      name,
			labels:    labels,
			lastValue: lastVal,
			sparkline: RenderSparkline(vals, 20),
			values:    vals,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].name != rows[j].name {
			return rows[i].name < rows[j].name
		}
		return rows[i].labels < rows[j].labels
	})

	return rows, nil
}

// parseVectorResults decodes an instant vector result into display rows.
func parseVectorResults(resp *QueryResponse) ([]seriesRow, error) {
	if resp.Data.ResultType != "vector" {
		return nil, fmt.Errorf("expected vector, got %s", resp.Data.ResultType)
	}

	var results []VectorResult
	if err := json.Unmarshal(resp.Data.Result, &results); err != nil {
		return nil, err
	}

	rows := make([]seriesRow, 0, len(results))
	for _, r := range results {
		name := r.Metric["__name__"]
		labels := formatLabels(r.Metric)
		val := r.Value.Float()

		rows = append(rows, seriesRow{
			name:      name,
			labels:    labels,
			lastValue: fmt.Sprintf("%.4g", val),
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].name != rows[j].name {
			return rows[i].name < rows[j].name
		}
		return rows[i].labels < rows[j].labels
	})

	return rows, nil
}

// formatLabels formats metric labels for display, excluding __name__.
func formatLabels(m map[string]string) string {
	parts := make([]string, 0, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == "__name__" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, " ")
}
