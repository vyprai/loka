package tsdb

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ExprType identifies the kind of parsed expression.
type ExprType int

const (
	ExprSelector    ExprType = iota // bare metric selector
	ExprFunction                    // rate(), delta(), increase(), etc.
	ExprAggregation                 // sum() by (), avg() by (), etc.
)

// Expr is the top-level parsed expression.
type Expr struct {
	Type        ExprType
	Selector    *Selector         // for plain metric selectors
	Function    *FunctionCall     // for rate(), delta(), etc.
	Aggregation *AggregationExpr  // for sum() by (), etc.
}

// Selector is a metric name with optional label matchers.
type Selector struct {
	Name     string
	Matchers []LabelMatcher
}

// FunctionCall represents a range function or histogram_quantile call.
type FunctionCall struct {
	Name     string        // rate, delta, increase, avg_over_time, etc.
	Selector *Selector
	Range    time.Duration // the [5m] part; zero for histogram_quantile
	Args     []float64     // leading numeric arguments (e.g. quantile value)
}

// AggregationExpr represents an aggregation operator with optional group-by.
type AggregationExpr struct {
	Op       string   // sum, avg, count
	Selector *Selector
	By       []string // group-by label names
}

// parser holds state for a single ParseExpr invocation.
type parser struct {
	input string
	pos   int
}

var rangeFunctions = map[string]bool{
	"rate":          true,
	"delta":         true,
	"increase":      true,
	"avg_over_time": true,
	"max_over_time": true,
	"min_over_time": true,
}

var aggregationOps = map[string]bool{
	"sum":   true,
	"avg":   true,
	"count": true,
}

// ParseExpr parses a PromQL subset expression into an Expr.
func ParseExpr(input string) (*Expr, error) {
	p := &parser{input: strings.TrimSpace(input)}
	expr, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	p.skipWhitespace()
	if p.pos < len(p.input) {
		return nil, fmt.Errorf("unexpected trailing input at position %d: %q", p.pos, p.input[p.pos:])
	}
	return expr, nil
}

func (p *parser) parseExpr() (*Expr, error) {
	p.skipWhitespace()
	if p.pos >= len(p.input) {
		return nil, fmt.Errorf("empty expression")
	}

	// Read the leading identifier.
	ident := p.readIdentifier()
	if ident == "" {
		return nil, fmt.Errorf("expected identifier at position %d", p.pos)
	}

	p.skipWhitespace()

	// Decide what kind of expression this is based on the identifier and
	// what follows it.
	if p.peek() == '(' {
		// It's a function or aggregation call.
		if ident == "histogram_quantile" {
			return p.parseHistogramQuantile()
		}
		if rangeFunctions[ident] {
			return p.parseRangeFunction(ident)
		}
		if aggregationOps[ident] {
			return p.parseAggregation(ident)
		}
		return nil, fmt.Errorf("unknown function or operator: %s", ident)
	}

	// Otherwise it's a plain selector, possibly with {}.
	sel, err := p.parseSelector(ident)
	if err != nil {
		return nil, err
	}
	return &Expr{Type: ExprSelector, Selector: sel}, nil
}

// parseSelector parses an optional label matcher block after we already have
// the metric name.
func (p *parser) parseSelector(name string) (*Selector, error) {
	sel := &Selector{Name: name}
	p.skipWhitespace()
	if p.peek() == '{' {
		matchers, err := p.parseLabelMatchers()
		if err != nil {
			return nil, err
		}
		sel.Matchers = matchers
	}
	return sel, nil
}

// parseLabelMatchers parses {key="val", key2=~"regex"}.
func (p *parser) parseLabelMatchers() ([]LabelMatcher, error) {
	if err := p.expect('{'); err != nil {
		return nil, err
	}
	var matchers []LabelMatcher
	for {
		p.skipWhitespace()
		if p.peek() == '}' {
			p.pos++
			return matchers, nil
		}
		if len(matchers) > 0 {
			if err := p.expect(','); err != nil {
				return nil, err
			}
			p.skipWhitespace()
		}
		m, err := p.parseLabelMatcher()
		if err != nil {
			return nil, err
		}
		matchers = append(matchers, m)
	}
}

func (p *parser) parseLabelMatcher() (LabelMatcher, error) {
	name := p.readIdentifier()
	if name == "" {
		return LabelMatcher{}, fmt.Errorf("expected label name at position %d", p.pos)
	}
	p.skipWhitespace()

	mt, err := p.parseMatchType()
	if err != nil {
		return LabelMatcher{}, err
	}

	p.skipWhitespace()
	val, err := p.parseQuotedString()
	if err != nil {
		return LabelMatcher{}, err
	}

	return LabelMatcher{Name: name, Value: val, Type: mt}, nil
}

func (p *parser) parseMatchType() (MatchType, error) {
	if p.pos >= len(p.input) {
		return 0, fmt.Errorf("expected match operator, got EOF")
	}
	// Try two-character operators first.
	if p.pos+1 < len(p.input) {
		two := p.input[p.pos : p.pos+2]
		switch two {
		case "!=":
			p.pos += 2
			return MatchNotEqual, nil
		case "=~":
			p.pos += 2
			return MatchRegexp, nil
		case "!~":
			p.pos += 2
			return MatchNotRegexp, nil
		}
	}
	if p.input[p.pos] == '=' {
		p.pos++
		return MatchEqual, nil
	}
	return 0, fmt.Errorf("expected match operator at position %d, got %q", p.pos, string(p.input[p.pos]))
}

func (p *parser) parseQuotedString() (string, error) {
	if err := p.expect('"'); err != nil {
		return "", fmt.Errorf("expected quoted string at position %d", p.pos)
	}
	var b strings.Builder
	for p.pos < len(p.input) {
		ch := p.input[p.pos]
		if ch == '\\' && p.pos+1 < len(p.input) {
			p.pos++
			next := p.input[p.pos]
			switch next {
			case '"', '\\':
				b.WriteByte(next)
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte('\\')
				b.WriteByte(next)
			}
			p.pos++
			continue
		}
		if ch == '"' {
			p.pos++
			return b.String(), nil
		}
		b.WriteByte(ch)
		p.pos++
	}
	return "", fmt.Errorf("unterminated string literal")
}

// parseRangeFunction parses: rate(selector[duration])
func (p *parser) parseRangeFunction(name string) (*Expr, error) {
	if err := p.expect('('); err != nil {
		return nil, err
	}
	p.skipWhitespace()

	metricName := p.readIdentifier()
	if metricName == "" {
		return nil, fmt.Errorf("expected metric name inside %s()", name)
	}

	sel, err := p.parseSelector(metricName)
	if err != nil {
		return nil, err
	}

	p.skipWhitespace()
	dur, err := p.parseRangeBracket()
	if err != nil {
		return nil, err
	}

	p.skipWhitespace()
	if err := p.expect(')'); err != nil {
		return nil, err
	}

	return &Expr{
		Type: ExprFunction,
		Function: &FunctionCall{
			Name:     name,
			Selector: sel,
			Range:    dur,
		},
	}, nil
}

// parseRangeBracket parses [5m], [1h], [30s], etc.
func (p *parser) parseRangeBracket() (time.Duration, error) {
	if err := p.expect('['); err != nil {
		return 0, err
	}
	start := p.pos
	for p.pos < len(p.input) && p.input[p.pos] != ']' {
		p.pos++
	}
	if p.pos >= len(p.input) {
		return 0, fmt.Errorf("unterminated range bracket")
	}
	raw := p.input[start:p.pos]
	p.pos++ // skip ']'

	dur, err := parseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid range duration %q: %w", raw, err)
	}
	return dur, nil
}

// parseDuration parses Prometheus-style durations: 5m, 1h, 30s, 1d, etc.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return 0, fmt.Errorf("empty duration")
	}

	var total time.Duration
	i := 0
	for i < len(s) {
		// Read numeric part.
		numStart := i
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
			i++
		}
		if i == numStart || i >= len(s) {
			return 0, fmt.Errorf("invalid duration: %q", s)
		}
		numStr := s[numStart:i]
		val, err := strconv.ParseFloat(numStr, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration number %q: %w", numStr, err)
		}

		unit := s[i]
		i++
		switch unit {
		case 's':
			total += time.Duration(val * float64(time.Second))
		case 'm':
			total += time.Duration(val * float64(time.Minute))
		case 'h':
			total += time.Duration(val * float64(time.Hour))
		case 'd':
			total += time.Duration(val * 24 * float64(time.Hour))
		case 'w':
			total += time.Duration(val * 7 * 24 * float64(time.Hour))
		case 'y':
			total += time.Duration(val * 365 * 24 * float64(time.Hour))
		default:
			return 0, fmt.Errorf("unknown duration unit %q", string(unit))
		}
	}
	return total, nil
}

// parseAggregation parses: sum(selector) by (label1, label2)
func (p *parser) parseAggregation(op string) (*Expr, error) {
	if err := p.expect('('); err != nil {
		return nil, err
	}
	p.skipWhitespace()

	metricName := p.readIdentifier()
	if metricName == "" {
		return nil, fmt.Errorf("expected metric name inside %s()", op)
	}

	sel, err := p.parseSelector(metricName)
	if err != nil {
		return nil, err
	}

	p.skipWhitespace()
	if err := p.expect(')'); err != nil {
		return nil, err
	}

	// Parse optional "by (...)" clause.
	var by []string
	p.skipWhitespace()
	if strings.HasPrefix(p.input[p.pos:], "by") {
		ahead := p.pos + 2
		// Make sure "by" is followed by whitespace or '(' (not part of a longer identifier).
		if ahead >= len(p.input) || !isIdentChar(p.input[ahead]) {
			p.pos = ahead
			p.skipWhitespace()
			by, err = p.parseLabelList()
			if err != nil {
				return nil, err
			}
		}
	}

	return &Expr{
		Type: ExprAggregation,
		Aggregation: &AggregationExpr{
			Op:       op,
			Selector: sel,
			By:       by,
		},
	}, nil
}

func (p *parser) parseLabelList() ([]string, error) {
	if err := p.expect('('); err != nil {
		return nil, err
	}
	var labels []string
	for {
		p.skipWhitespace()
		if p.peek() == ')' {
			p.pos++
			return labels, nil
		}
		if len(labels) > 0 {
			if err := p.expect(','); err != nil {
				return nil, err
			}
			p.skipWhitespace()
		}
		name := p.readIdentifier()
		if name == "" {
			return nil, fmt.Errorf("expected label name at position %d", p.pos)
		}
		labels = append(labels, name)
	}
}

// parseHistogramQuantile parses: histogram_quantile(0.95, selector{...})
func (p *parser) parseHistogramQuantile() (*Expr, error) {
	if err := p.expect('('); err != nil {
		return nil, err
	}
	p.skipWhitespace()

	// Parse the quantile value (a float).
	quantile, err := p.parseFloat()
	if err != nil {
		return nil, fmt.Errorf("expected quantile value: %w", err)
	}

	p.skipWhitespace()
	if err := p.expect(','); err != nil {
		return nil, err
	}
	p.skipWhitespace()

	metricName := p.readIdentifier()
	if metricName == "" {
		return nil, fmt.Errorf("expected metric name inside histogram_quantile()")
	}

	sel, err := p.parseSelector(metricName)
	if err != nil {
		return nil, err
	}

	p.skipWhitespace()
	if err := p.expect(')'); err != nil {
		return nil, err
	}

	return &Expr{
		Type: ExprFunction,
		Function: &FunctionCall{
			Name:     "histogram_quantile",
			Selector: sel,
			Args:     []float64{quantile},
		},
	}, nil
}

func (p *parser) parseFloat() (float64, error) {
	start := p.pos
	if p.pos < len(p.input) && (p.input[p.pos] == '-' || p.input[p.pos] == '+') {
		p.pos++
	}
	hasDot := false
	for p.pos < len(p.input) {
		ch := p.input[p.pos]
		if ch >= '0' && ch <= '9' {
			p.pos++
		} else if ch == '.' && !hasDot {
			hasDot = true
			p.pos++
		} else {
			break
		}
	}
	if p.pos == start {
		return 0, fmt.Errorf("expected number at position %d", p.pos)
	}
	return strconv.ParseFloat(p.input[start:p.pos], 64)
}

// --- low-level helpers ---

func (p *parser) skipWhitespace() {
	for p.pos < len(p.input) && unicode.IsSpace(rune(p.input[p.pos])) {
		p.pos++
	}
}

func (p *parser) peek() byte {
	if p.pos >= len(p.input) {
		return 0
	}
	return p.input[p.pos]
}

func (p *parser) expect(ch byte) error {
	if p.pos >= len(p.input) {
		return fmt.Errorf("expected %q, got EOF", string(ch))
	}
	if p.input[p.pos] != ch {
		return fmt.Errorf("expected %q at position %d, got %q", string(ch), p.pos, string(p.input[p.pos]))
	}
	p.pos++
	return nil
}

func (p *parser) readIdentifier() string {
	if p.pos >= len(p.input) {
		return ""
	}
	ch := p.input[p.pos]
	// Identifiers must start with a letter or underscore.
	if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_') {
		return ""
	}
	start := p.pos
	for p.pos < len(p.input) && isIdentChar(p.input[p.pos]) {
		p.pos++
	}
	return p.input[start:p.pos]
}

func isIdentChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') || ch == '_'
}
