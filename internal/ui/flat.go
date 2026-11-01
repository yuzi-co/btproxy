package ui

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// The settings file and the status endpoint are flat JSON objects of strings. encoding/json would add
// about 850 KB to the binary (it is not linked otherwise), so this small codec does instead.

// EncodeFlat writes m as a JSON object with sorted keys, one per line.
func EncodeFlat(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n  ")
		quoteJSON(&b, k)
		b.WriteString(": ")
		quoteJSON(&b, m[k])
	}
	if len(keys) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

func quoteJSON(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == '<' || r == '>' || r == '&' || r == 0x2028 || r == 0x2029:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// DecodeFlat parses a JSON object whose values are strings, numbers, booleans or null; non-string values
// are kept as their literal text ("3", "true"), null as "". Nested objects and arrays are rejected.
func DecodeFlat(data []byte) (map[string]string, error) {
	p := &flatParser{s: string(data)}
	m := map[string]string{}
	p.ws()
	if !p.eat('{') {
		return nil, p.err("expected {")
	}
	p.ws()
	if p.eat('}') {
		return m, p.end()
	}
	for {
		p.ws()
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.ws()
		if !p.eat(':') {
			return nil, p.err("expected :")
		}
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		m[k] = v
		p.ws()
		if p.eat('}') {
			return m, p.end()
		}
		if !p.eat(',') {
			return nil, p.err("expected , or }")
		}
	}
}

type flatParser struct {
	s string
	i int
}

func (p *flatParser) err(msg string) error { return fmt.Errorf("json: %s at offset %d", msg, p.i) }

func (p *flatParser) ws() {
	for p.i < len(p.s) && strings.IndexByte(" \t\r\n", p.s[p.i]) >= 0 {
		p.i++
	}
}

func (p *flatParser) eat(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *flatParser) end() error {
	p.ws()
	if p.i != len(p.s) {
		return p.err("trailing data")
	}
	return nil
}

func (p *flatParser) value() (string, error) {
	if p.i < len(p.s) && p.s[p.i] == '"' {
		return p.str()
	}
	start := p.i
	for p.i < len(p.s) && strings.IndexByte(",} \t\r\n", p.s[p.i]) < 0 {
		p.i++
	}
	lit := p.s[start:p.i]
	switch {
	case lit == "null":
		return "", nil
	case lit == "true" || lit == "false":
		return lit, nil
	case lit != "" && strings.Trim(lit, "+-.eE0123456789") == "":
		return lit, nil
	}
	p.i = start
	return "", p.err("expected a string, number, boolean or null")
}

func (p *flatParser) str() (string, error) {
	if !p.eat('"') {
		return "", p.err("expected a string")
	}
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c < 0x20:
			return "", p.err("control character in string")
		case c == '\\':
			if p.i+1 >= len(p.s) {
				return "", p.err("bad escape")
			}
			e := p.s[p.i+1]
			p.i += 2
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r, ok := p.hex4()
				if !ok {
					return "", p.err("bad \\u escape")
				}
				if utf16.IsSurrogate(r) && strings.HasPrefix(p.s[p.i:], `\u`) {
					p.i += 2
					r2, ok := p.hex4()
					if !ok {
						return "", p.err("bad \\u escape")
					}
					r = utf16.DecodeRune(r, r2)
				}
				b.WriteRune(r)
			default:
				return "", p.err("bad escape")
			}
		default:
			r, n := utf8.DecodeRuneInString(p.s[p.i:])
			b.WriteRune(r)
			p.i += n
		}
	}
	return "", errors.New("json: unterminated string")
}

func (p *flatParser) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	var r rune
	for _, c := range p.s[p.i : p.i+4] {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | (c - '0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | (c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | (c - 'A' + 10)
		default:
			return 0, false
		}
	}
	p.i += 4
	return r, true
}
