// Package notificationtemplate renders the user-configured notification
// templates stored by MoviePilot V2.
package notificationtemplate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/moviepilot-go/moviepilot-go/internal/notification"
	"github.com/nikolalohinski/gonja/v2"
	gonjaexec "github.com/nikolalohinski/gonja/v2/exec"
)

// Renderer holds a detached NotificationTemplates configuration.
type Renderer struct {
	templates map[string]any
}

// New decodes the NotificationTemplates dictionary. Its values may use JSON,
// Python literal dictionaries (including Python's adjacent string literals),
// or plain Jinja template strings.
func New(raw json.RawMessage) (*Renderer, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return &Renderer{templates: map[string]any{}}, nil
	}
	var configured map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &configured); err != nil {
		return nil, fmt.Errorf("decode notification templates: %w", err)
	}
	if configured == nil {
		return &Renderer{templates: map[string]any{}}, nil
	}
	renderer := &Renderer{templates: make(map[string]any, len(configured))}
	for contentType, rawTemplate := range configured {
		value, err := decodeTemplateValue(rawTemplate)
		if err != nil {
			return nil, fmt.Errorf("decode notification template %q: %w", contentType, err)
		}
		renderer.templates[contentType] = value
	}
	return renderer, nil
}

// Apply renders the configured template for msg.ContentType. Messages without
// a matching template are returned unchanged.
func (r *Renderer) Apply(msg notification.Message, context map[string]any) (notification.Message, error) {
	if r == nil || msg.ContentType == "" || msg.Title != "" || msg.Text != "" {
		return msg, nil
	}
	templateValue, exists := r.templates[string(msg.ContentType)]
	if !exists {
		return msg, nil
	}
	values, ok := templateValue.(map[string]any)
	if !ok {
		return msg, fmt.Errorf("notification template %q must render to a dictionary", msg.ContentType)
	}
	copy := msg
	for key, rawValue := range values {
		text, ok := rawValue.(string)
		if !ok {
			continue
		}
		rendered, err := renderString(text, context)
		if err != nil {
			return msg, fmt.Errorf("render notification template %q field %q: %w", msg.ContentType, key, err)
		}
		rendered = restoreFormattedEscapes(rendered)
		switch strings.ToLower(key) {
		case "title":
			copy.Title = rendered
		case "text":
			copy.Text = rendered
		case "image":
			copy.Image = rendered
		case "link":
			copy.Link = rendered
		}
	}
	return copy, nil
}

func restoreFormattedEscapes(value string) string {
	return strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\b`, "\b", `\f`, "\f").Replace(value)
}

func decodeTemplateValue(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("empty template")
	}
	var value any
	if json.Unmarshal(trimmed, &value) == nil {
		if text, ok := value.(string); ok {
			var object any
			if json.Unmarshal([]byte(text), &object) == nil {
				if _, ok := object.(map[string]any); ok {
					return object, nil
				}
			}
			if parsed, err := parsePythonLiteral(text); err == nil {
				return parsed, nil
			}
			return map[string]any{"text": text}, nil
		}
		if object, ok := value.(map[string]any); ok {
			return object, nil
		}
		return nil, errors.New("template must be a string or dictionary")
	}

	// A few installations keep the template body as a Python literal string
	// without JSON-encoding the body itself.
	parsed, err := parsePythonLiteral(string(trimmed))
	if err != nil {
		return nil, err
	}
	if _, ok := parsed.(map[string]any); !ok {
		return nil, errors.New("Python literal template must be a dictionary")
	}
	return parsed, nil
}

func renderString(source string, context map[string]any) (string, error) {
	template, err := gonja.FromString(source)
	if err != nil {
		return "", err
	}
	if context == nil {
		context = map[string]any{}
	}
	return template.ExecuteToString(gonjaexec.NewContext(context))
}

// parsePythonLiteral implements the data-only subset used by ast.literal_eval:
// dictionaries, lists/tuples, strings, numbers, booleans, and None. Adjacent
// quoted strings concatenate according to Python literal rules.
func parsePythonLiteral(source string) (any, error) {
	p := literalParser{source: source}
	value, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.pos != len(p.source) {
		return nil, fmt.Errorf("unexpected character at offset %d", p.pos)
	}
	return value, nil
}

type literalParser struct {
	source string
	pos    int
}

func (p *literalParser) parseValue() (any, error) {
	p.skipSpace()
	if p.pos >= len(p.source) {
		return nil, errors.New("unexpected end of Python literal")
	}
	switch p.source[p.pos] {
	case '{':
		return p.parseDict()
	case '[':
		return p.parseSequence('[', ']')
	case '(':
		return p.parseSequence('(', ')')
	case '\'', '"':
		return p.parseStringConcat()
	default:
		return p.parseAtom()
	}
}

func (p *literalParser) parseDict() (map[string]any, error) {
	p.pos++
	result := map[string]any{}
	p.skipSpace()
	if p.consume('}') {
		return result, nil
	}
	for {
		p.skipSpace()
		keyValue, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		key, ok := keyValue.(string)
		if !ok {
			return nil, errors.New("Python template dictionary keys must be strings")
		}
		p.skipSpace()
		if !p.consume(':') {
			return nil, fmt.Errorf("expected ':' at offset %d", p.pos)
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		result[key] = value
		p.skipSpace()
		if p.consume('}') {
			return result, nil
		}
		if !p.consume(',') {
			return nil, fmt.Errorf("expected ',' or '}' at offset %d", p.pos)
		}
		p.skipSpace()
		if p.consume('}') {
			return result, nil
		}
	}
}

func (p *literalParser) parseSequence(open, close byte) ([]any, error) {
	p.pos++
	values := []any{}
	p.skipSpace()
	if p.consume(close) {
		return values, nil
	}
	for {
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		p.skipSpace()
		if p.consume(close) {
			return values, nil
		}
		if !p.consume(',') {
			return nil, fmt.Errorf("expected ',' or %q at offset %d", close, p.pos)
		}
		p.skipSpace()
		if p.consume(close) {
			return values, nil
		}
	}
}

func (p *literalParser) parseStringConcat() (string, error) {
	var result strings.Builder
	for {
		p.skipSpace()
		if p.pos >= len(p.source) || (p.source[p.pos] != '\'' && p.source[p.pos] != '"') {
			break
		}
		part, err := p.parseString()
		if err != nil {
			return "", err
		}
		result.WriteString(part)
	}
	return result.String(), nil
}

func (p *literalParser) parseString() (string, error) {
	quote := p.source[p.pos]
	p.pos++
	triple := p.pos+1 < len(p.source) && p.source[p.pos] == quote && p.source[p.pos+1] == quote
	if triple {
		p.pos += 2
	}
	var result strings.Builder
	for p.pos < len(p.source) {
		current := p.source[p.pos]
		if current == quote {
			if !triple {
				p.pos++
				return result.String(), nil
			}
			if p.pos+2 < len(p.source) && p.source[p.pos+1] == quote && p.source[p.pos+2] == quote {
				p.pos += 3
				return result.String(), nil
			}
			result.WriteByte(current)
			p.pos++
			continue
		}
		p.pos++
		if current != '\\' {
			result.WriteByte(current)
			continue
		}
		if p.pos >= len(p.source) {
			return "", errors.New("unterminated escape sequence")
		}
		escaped := p.source[p.pos]
		p.pos++
		switch escaped {
		case '\n':
			continue
		case '\r':
			if p.pos < len(p.source) && p.source[p.pos] == '\n' {
				p.pos++
			}
			continue
		case 'n':
			result.WriteByte('\n')
		case 'r':
			result.WriteByte('\r')
		case 't':
			result.WriteByte('\t')
		case 'b':
			result.WriteByte('\b')
		case 'f':
			result.WriteByte('\f')
		case 'v':
			result.WriteByte('\v')
		case 'a':
			result.WriteByte('\a')
		case '\\':
			result.WriteByte('\\')
		case '\'', '"':
			result.WriteByte(escaped)
		case 'x', 'u', 'U':
			digits := 2
			if escaped == 'u' {
				digits = 4
			}
			if escaped == 'U' {
				digits = 8
			}
			if p.pos+digits > len(p.source) {
				return "", errors.New("short hexadecimal escape")
			}
			codepoint, err := strconv.ParseUint(p.source[p.pos:p.pos+digits], 16, 32)
			if err != nil || codepoint > 0x10ffff || (codepoint >= 0xd800 && codepoint <= 0xdfff) {
				return "", errors.New("invalid hexadecimal escape")
			}
			result.WriteRune(rune(codepoint))
			p.pos += digits
		default:
			// Python preserves unknown escapes as a backslash and character.
			result.WriteByte('\\')
			result.WriteByte(escaped)
		}
	}
	return "", errors.New("unterminated Python string literal")
}

func (p *literalParser) parseAtom() (any, error) {
	start := p.pos
	for p.pos < len(p.source) && !strings.ContainsRune(" \t\r\n,:{}[]()", rune(p.source[p.pos])) {
		p.pos++
	}
	if start == p.pos {
		return nil, fmt.Errorf("invalid literal at offset %d", p.pos)
	}
	atom := p.source[start:p.pos]
	switch atom {
	case "True":
		return true, nil
	case "False":
		return false, nil
	case "None":
		return nil, nil
	}
	if integer, err := strconv.ParseInt(atom, 0, 64); err == nil {
		return integer, nil
	}
	if number, err := strconv.ParseFloat(atom, 64); err == nil {
		return number, nil
	}
	return nil, fmt.Errorf("unsupported Python literal %q", atom)
}

func (p *literalParser) skipSpace() {
	for p.pos < len(p.source) {
		switch p.source[p.pos] {
		case ' ', '\t', '\r', '\n':
			p.pos++
		case '#':
			for p.pos < len(p.source) && p.source[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *literalParser) consume(value byte) bool {
	if p.pos < len(p.source) && p.source[p.pos] == value {
		p.pos++
		return true
	}
	return false
}
