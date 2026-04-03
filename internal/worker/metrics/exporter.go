package workermetrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/vyprai/loka/internal/metrics"
)

// Exporter serves the /metrics endpoint in Prometheus text exposition format.
type Exporter struct {
	scraper *Scraper
}

// NewExporter creates a new metrics exporter.
func NewExporter(scraper *Scraper) *Exporter {
	return &Exporter{scraper: scraper}
}

// ServeHTTP handles GET /metrics requests.
func (e *Exporter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	points := e.scraper.GetPoints()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Group points by metric name for TYPE/HELP comments.
	grouped := make(map[string][]metrics.DataPoint)
	var names []string
	for _, p := range points {
		if _, ok := grouped[p.Name]; !ok {
			names = append(names, p.Name)
		}
		grouped[p.Name] = append(grouped[p.Name], p)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		pts := grouped[name]
		if len(pts) == 0 {
			continue
		}
		// TYPE comment.
		typeStr := "gauge"
		switch pts[0].Type {
		case metrics.Counter:
			typeStr = "counter"
		case metrics.Histogram:
			typeStr = "histogram"
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, typeStr)

		for _, p := range pts {
			writeMetricLine(&b, p)
		}
	}

	w.Write([]byte(b.String()))
}

func writeMetricLine(b *strings.Builder, p metrics.DataPoint) {
	b.WriteString(p.Name)
	if len(p.Labels) > 0 {
		b.WriteByte('{')
		for i, l := range p.Labels {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(b, "%s=\"%s\"", l.Name, escapeLabel(l.Value))
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	if math.IsNaN(p.Value) {
		b.WriteString("NaN")
	} else if math.IsInf(p.Value, 1) {
		b.WriteString("+Inf")
	} else if math.IsInf(p.Value, -1) {
		b.WriteString("-Inf")
	} else {
		fmt.Fprintf(b, "%g", p.Value)
	}
	b.WriteByte('\n')
}

func escapeLabel(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return s
}
