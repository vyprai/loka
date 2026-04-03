package logql

import (
	"fmt"
	"strings"
	"unicode"
)

// parser holds state for a single Parse invocation.
type parser struct {
	input string
	pos   int
}

// Parse parses a LogQL query string into a LogQuery.
func Parse(input string) (*LogQuery, error) {
	p := &parser{input: strings.TrimSpace(input)}
	q, err := p.parseQuery()
	if err != nil {
		return nil, err
	}
	p.skipWhitespace()
	if p.pos < len(p.input) {
		return nil, fmt.Errorf("unexpected trailing input at position %d: %q", p.pos, p.input[p.pos:])
	}
	return q, nil
}

func (p *parser) parseQuery() (*LogQuery, error) {
	p.skipWhitespace()
	if p.pos >= len(p.input) {
		return nil, fmt.Errorf("empty query")
	}

	matchers, err := p.parseLabelMatchers()
	if err != nil {
		return nil, err
	}

	filters, err := p.parsePipeline()
	if err != nil {
		return nil, err
	}

	return &LogQuery{
		Matchers: matchers,
		Filters:  filters,
	}, nil
}

// parseLabelMatchers parses {key="val", key2=~"regex"}.
func (p *parser) parseLabelMatchers() ([]LabelMatcher, error) {
	if err := p.expect('{'); err != nil {
		return nil, fmt.Errorf("expected '{' at start of selector, got %q", p.peekString())
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

// parsePipeline parses zero or more line filters after the selector.
func (p *parser) parsePipeline() ([]LineFilter, error) {
	var filters []LineFilter
	for {
		p.skipWhitespace()
		if p.pos >= len(p.input) {
			return filters, nil
		}

		ft, ok := p.tryLineFilterOp()
		if !ok {
			return filters, nil
		}

		p.skipWhitespace()
		pattern, err := p.parseQuotedString()
		if err != nil {
			return nil, err
		}

		filters = append(filters, LineFilter{Type: ft, Pattern: pattern})
	}
}

// tryLineFilterOp attempts to consume a line filter operator and returns its type.
func (p *parser) tryLineFilterOp() (LineFilterType, bool) {
	if p.pos+1 >= len(p.input) {
		return 0, false
	}
	two := p.input[p.pos : p.pos+2]
	switch two {
	case "|=":
		p.pos += 2
		return FilterContains, true
	case "|~":
		p.pos += 2
		return FilterRegex, true
	case "!=":
		p.pos += 2
		return FilterNotContains, true
	case "!~":
		p.pos += 2
		return FilterNotRegex, true
	}
	return 0, false
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

func (p *parser) peekString() string {
	if p.pos >= len(p.input) {
		return "EOF"
	}
	return string(p.input[p.pos])
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
