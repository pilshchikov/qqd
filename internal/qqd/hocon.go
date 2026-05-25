package qqd

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type tokenType int

const (
	tokenEOF tokenType = iota
	tokenIdent
	tokenString
	tokenNumber
	tokenBool
	tokenLBrace
	tokenRBrace
	tokenLBracket
	tokenRBracket
	tokenEquals
	tokenComma
)

type token struct {
	typ  tokenType
	text string
	pos  int
}

type lexer struct {
	in  []rune
	pos int
}

// newLexer constructs a rune-based lexer for a HOCON-like source.
func newLexer(input string) *lexer {
	return &lexer{in: []rune(input)}
}

// eof reports whether the lexer consumed the full input.
func (l *lexer) eof() bool {
	return l.pos >= len(l.in)
}

// peek returns the current rune without advancing.
func (l *lexer) peek() rune {
	if l.eof() {
		return 0
	}
	return l.in[l.pos]
}

// peekN returns the rune at current position + n without advancing.
func (l *lexer) peekN(n int) rune {
	idx := l.pos + n
	if idx >= len(l.in) {
		return 0
	}
	return l.in[idx]
}

// advance consumes and returns the current rune.
func (l *lexer) advance() rune {
	if l.eof() {
		return 0
	}
	ch := l.in[l.pos]
	l.pos++
	return ch
}

// skipWSAndComments consumes whitespace and line comments.
func (l *lexer) skipWSAndComments() {
	for !l.eof() {
		ch := l.peek()
		switch {
		case unicode.IsSpace(ch):
			l.advance()
		case ch == '#':
			for !l.eof() && l.peek() != '\n' {
				l.advance()
			}
		case ch == '/' && l.peekN(1) == '/':
			l.advance()
			l.advance()
			for !l.eof() && l.peek() != '\n' {
				l.advance()
			}
		default:
			return
		}
	}
}

// nextToken returns the next lexical token.
func (l *lexer) nextToken() (token, error) {
	l.skipWSAndComments()
	start := l.pos
	if l.eof() {
		return token{typ: tokenEOF, pos: start}, nil
	}
	switch ch := l.advance(); ch {
	case '{':
		return token{typ: tokenLBrace, text: "{", pos: start}, nil
	case '}':
		return token{typ: tokenRBrace, text: "}", pos: start}, nil
	case '[':
		return token{typ: tokenLBracket, text: "[", pos: start}, nil
	case ']':
		return token{typ: tokenRBracket, text: "]", pos: start}, nil
	case '=', ':':
		return token{typ: tokenEquals, text: string(ch), pos: start}, nil
	case ',':
		return token{typ: tokenComma, text: ",", pos: start}, nil
	case '"':
		var b strings.Builder
		for !l.eof() {
			c := l.advance()
			if c == '"' {
				return token{typ: tokenString, text: b.String(), pos: start}, nil
			}
			if c == '\\' {
				if l.eof() {
					return token{}, fmt.Errorf("unterminated escape at %d", start)
				}
				esc := l.advance()
				switch esc {
				case '"', '\\', '/':
					b.WriteRune(esc)
				case 'n':
					b.WriteRune('\n')
				case 'r':
					b.WriteRune('\r')
				case 't':
					b.WriteRune('\t')
				default:
					return token{}, fmt.Errorf("unsupported escape \\%c at %d", esc, l.pos)
				}
				continue
			}
			b.WriteRune(c)
		}
		return token{}, fmt.Errorf("unterminated string at %d", start)
	default:
		var b strings.Builder
		b.WriteRune(ch)
		for !l.eof() {
			n := l.peek()
			if unicode.IsSpace(n) {
				break
			}
			if strings.ContainsRune("{}[]=,:#", n) {
				break
			}
			if n == '/' && l.peekN(1) == '/' {
				break
			}
			b.WriteRune(l.advance())
		}
		text := b.String()
		if text == "true" || text == "false" {
			return token{typ: tokenBool, text: text, pos: start}, nil
		}
		if _, err := strconv.ParseInt(text, 10, 64); err == nil {
			return token{typ: tokenNumber, text: text, pos: start}, nil
		}
		if _, err := strconv.ParseFloat(text, 64); err == nil && strings.ContainsRune(text, '.') {
			return token{typ: tokenNumber, text: text, pos: start}, nil
		}
		return token{typ: tokenIdent, text: text, pos: start}, nil
	}
}

type parser struct {
	lx  *lexer
	tok token
}

// newParser creates a parser and primes the first token.
func newParser(input string) (*parser, error) {
	p := &parser{lx: newLexer(input)}
	if err := p.next(); err != nil {
		return nil, err
	}
	return p, nil
}

// next advances the parser to the next token.
func (p *parser) next() error {
	tok, err := p.lx.nextToken()
	if err != nil {
		return err
	}
	p.tok = tok
	return nil
}

// parseRoot parses the top-level object.
func (p *parser) parseRoot() (map[string]any, error) {
	return p.parseObject(tokenEOF)
}

// parseObject parses key/value pairs until the end token is reached.
func (p *parser) parseObject(end tokenType) (map[string]any, error) {
	out := map[string]any{}
	for p.tok.typ != end {
		if p.tok.typ == tokenEOF {
			if end == tokenEOF {
				return out, nil
			}
			return nil, fmt.Errorf("unexpected EOF")
		}
		if p.tok.typ == tokenComma {
			if err := p.next(); err != nil {
				return nil, err
			}
			continue
		}
		keys, err := p.parseKeyPath()
		if err != nil {
			return nil, err
		}
		switch p.tok.typ {
		case tokenEquals:
			if err := p.next(); err != nil {
				return nil, err
			}
			val, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			setPath(out, keys, val)
		case tokenLBrace:
			if err := p.next(); err != nil {
				return nil, err
			}
			val, err := p.parseObject(tokenRBrace)
			if err != nil {
				return nil, err
			}
			setPath(out, keys, val)
			if p.tok.typ != tokenRBrace {
				return nil, fmt.Errorf("expected }")
			}
			if err := p.next(); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("expected '=' or '{' after key at pos %d", p.tok.pos)
		}
	}
	return out, nil
}

// parseKeyPath parses a dotted key path.
// Quoted keys (tokenString) are treated as a single literal key — dots inside
// quotes are NOT path separators.  Only unquoted identifiers and numbers are
// split on '.'.
func (p *parser) parseKeyPath() ([]string, error) {
	if p.tok.typ != tokenIdent && p.tok.typ != tokenString && p.tok.typ != tokenNumber {
		return nil, fmt.Errorf("expected key, got %q at %d", p.tok.text, p.tok.pos)
	}
	key := p.tok.text
	quoted := p.tok.typ == tokenString
	if err := p.next(); err != nil {
		return nil, err
	}
	if quoted {
		// Quoted keys are literal — never split on dots.
		return []string{key}, nil
	}
	parts := strings.Split(key, ".")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty key path")
	}
	return out, nil
}

// parseValue parses one scalar/object/array value.
func (p *parser) parseValue() (any, error) {
	switch p.tok.typ {
	case tokenString, tokenIdent:
		val := p.tok.text
		if err := p.next(); err != nil {
			return nil, err
		}
		return val, nil
	case tokenNumber:
		txt := p.tok.text
		if err := p.next(); err != nil {
			return nil, err
		}
		if strings.ContainsRune(txt, '.') {
			f, _ := strconv.ParseFloat(txt, 64)
			return f, nil
		}
		i, _ := strconv.ParseInt(txt, 10, 64)
		return int(i), nil
	case tokenBool:
		val := p.tok.text == "true"
		if err := p.next(); err != nil {
			return nil, err
		}
		return val, nil
	case tokenLBrace:
		if err := p.next(); err != nil {
			return nil, err
		}
		obj, err := p.parseObject(tokenRBrace)
		if err != nil {
			return nil, err
		}
		if p.tok.typ != tokenRBrace {
			return nil, fmt.Errorf("expected }")
		}
		if err := p.next(); err != nil {
			return nil, err
		}
		return obj, nil
	case tokenLBracket:
		return p.parseArray()
	default:
		return nil, fmt.Errorf("unexpected token %q for value", p.tok.text)
	}
}

// parseArray parses bracketed array values.
func (p *parser) parseArray() ([]any, error) {
	// current token is '['
	if err := p.next(); err != nil {
		return nil, err
	}
	var out []any
	for p.tok.typ != tokenRBracket {
		if p.tok.typ == tokenEOF {
			return nil, fmt.Errorf("unexpected EOF in array")
		}
		if p.tok.typ == tokenComma {
			if err := p.next(); err != nil {
				return nil, err
			}
			continue
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if p.tok.typ == tokenComma {
			if err := p.next(); err != nil {
				return nil, err
			}
			continue
		}
	}
	if err := p.next(); err != nil {
		return nil, err
	}
	return out, nil
}

// setPath writes value into a nested map path, merging objects when needed.
func setPath(root map[string]any, keys []string, value any) {
	cur := root
	for i := 0; i < len(keys)-1; i++ {
		k := keys[i]
		next, ok := cur[k]
		if !ok {
			nm := map[string]any{}
			cur[k] = nm
			cur = nm
			continue
		}
		m, ok := next.(map[string]any)
		if !ok {
			m = map[string]any{}
			cur[k] = m
		}
		cur = m
	}
	last := keys[len(keys)-1]
	if old, ok := cur[last].(map[string]any); ok {
		if n, ok := value.(map[string]any); ok {
			cur[last] = deepMergeMaps(old, n)
			return
		}
	}
	cur[last] = value
}

// parseHOCON parses a relaxed HOCON-like text into nested maps.
func parseHOCON(input string) (map[string]any, error) {
	p, err := newParser(input)
	if err != nil {
		return nil, err
	}
	return p.parseRoot()
}
