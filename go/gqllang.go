package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// This file holds the GraphQL document parser: a lexer plus a recursive
// descent parser producing just enough AST for the suite's documents.

type gqlSelection struct {
	name       string
	aliasName  string
	args       map[string]any
	selections []gqlSelection
}

func (s gqlSelection) alias() string {
	if s.aliasName != "" {
		return s.aliasName
	}
	return s.name
}

type operation struct {
	kind         string // query | mutation | subscription
	name         string
	variableDefs map[string]any
	selections   []gqlSelection
}

type document struct {
	operations  []*operation
	fragments   map[string][]gqlSelection
	fragVarDefs map[string]map[string]any
}

func (d *document) operation(name string) *operation {
	if name == "" {
		if len(d.operations) == 1 {
			return d.operations[0]
		}
		if len(d.operations) == 0 {
			return nil
		}
		return nil
	}
	for _, op := range d.operations {
		if op.name == name {
			return op
		}
	}
	return nil
}

// ── lexer ───────────────────────────────────────────────────────────────────

type token struct {
	kind  string // name | number | string | punct | spread | eof
	text  string
	num   float64
	isInt bool
}

type lexer struct {
	src  string
	pos  int
	toks []token
}

func lex(src string) ([]token, error) {
	l := &lexer{src: src}
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			l.pos++
		case c == '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == '"':
			if err := l.lexString(); err != nil {
				return nil, err
			}
		case c == '.':
			if strings.HasPrefix(l.src[l.pos:], "...") {
				l.toks = append(l.toks, token{kind: "spread", text: "..."})
				l.pos += 3
			} else {
				return nil, errors.New("unexpected character \".\"")
			}
		case c == '-' || (c >= '0' && c <= '9'):
			l.lexNumber()
		case c == '_' || isNameStart(c):
			l.lexName()
		default:
			l.toks = append(l.toks, token{kind: "punct", text: string(c)})
			l.pos++
		}
	}
	l.toks = append(l.toks, token{kind: "eof"})
	return l.toks, nil
}

func isNameStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isNameChar(c byte) bool {
	return isNameStart(c) || c == '_' || c >= '0' && c <= '9'
}

func (l *lexer) lexName() {
	start := l.pos
	for l.pos < len(l.src) && isNameChar(l.src[l.pos]) {
		l.pos++
	}
	l.toks = append(l.toks, token{kind: "name", text: l.src[start:l.pos]})
}

func (l *lexer) lexNumber() {
	start := l.pos
	if l.src[l.pos] == '-' {
		l.pos++
	}
	isInt := true
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c >= '0' && c <= '9' {
			l.pos++
			continue
		}
		if c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			isInt = false
			l.pos++
			continue
		}
		break
	}
	text := l.src[start:l.pos]
	v, _ := strconv.ParseFloat(text, 64)
	l.toks = append(l.toks, token{kind: "number", text: text, num: v, isInt: isInt})
}

func (l *lexer) lexString() error {
	if strings.HasPrefix(l.src[l.pos:], `"""`) {
		end := strings.Index(l.src[l.pos+3:], `"""`)
		if end < 0 {
			return errors.New("unterminated block string")
		}
		text := l.src[l.pos+3 : l.pos+3+end]
		l.toks = append(l.toks, token{kind: "string", text: strings.TrimSpace(text)})
		l.pos += 3 + end + 3
		return nil
	}
	l.pos++ // opening quote
	var sb strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '\\' && l.pos+1 < len(l.src) {
			l.pos++
			switch l.src[l.pos] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			default:
				sb.WriteByte(l.src[l.pos])
			}
			l.pos++
			continue
		}
		if c == '"' {
			l.pos++
			l.toks = append(l.toks, token{kind: "string", text: sb.String()})
			return nil
		}
		sb.WriteByte(c)
		l.pos++
	}
	return errors.New("unterminated string")
}

// ── parser ──────────────────────────────────────────────────────────────────

type parser struct {
	toks []token
	pos  int
	frag map[string][]gqlSelection
}

func parseDocument(src string) (*document, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, frag: map[string][]gqlSelection{}}
	doc := &document{fragments: map[string][]gqlSelection{}, fragVarDefs: map[string]map[string]any{}}
	// Fragments may be defined after use, so collect them first.
	p.collectFragments()

	for !p.at("eof") {
		if p.atName("fragment") {
			p.skipFragment()
			continue
		}
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		doc.operations = append(doc.operations, op)
	}
	doc.fragments = p.frag
	doc.fragVarDefs = map[string]map[string]any{}
	return doc, nil
}

func (p *parser) collectFragments() {
	saved := p.pos
	for i := 0; i < len(p.toks); i++ {
		if p.toks[i].kind == "name" && p.toks[i].text == "fragment" {
			p.pos = i + 1
			if p.atName("on") {
				p.next()
			}
			if p.at("name") {
				p.next()
			}
			sels, _ := p.parseSelectionSet()
			p.frag[""] = sels
			_ = sels
		}
	}
	p.pos = saved
}

func (p *parser) skipFragment() {
	for !p.at("eof") && !p.atPunct("{") {
		p.next()
	}
	_, _ = p.parseSelectionSet()
}

func (p *parser) parseOperation() (*operation, error) {
	op := &operation{kind: "query", variableDefs: map[string]any{}}
	if p.atName("query") || p.atName("mutation") || p.atName("subscription") {
		op.kind = p.next().text
		if p.at("name") {
			op.name = p.next().text
		}
		if p.atPunct("(") {
			p.next()
			for !p.atPunct(")") && !p.at("eof") {
				if p.atPunct("$") {
					p.next()
				}
				if !p.at("name") {
					p.next()
					continue
				}
				name := p.next().text
				if p.atPunct(":") {
					p.next()
				}
				def := p.parseTypeRef()
				if p.atPunct("=") {
					p.next()
					def = p.parseValue()
				}
				op.variableDefs[name] = def
				p.skipDirectives()
			}
			p.expect(")")
		}
		p.skipDirectives()
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.selections = sels
	return op, nil
}

// parseTypeRef consumes a type reference such as String!, [String] or
// [Issue!]! and returns nothing: the executor does not resolve types, it only
// needs the parser to step over them so the variable name list stays correct.
func (p *parser) parseTypeRef() any {
	consume := func() {
		if p.at("name") {
			p.next()
		}
	}
	if p.atPunct("[") {
		p.next()
		consume()
		if p.atPunct("!") {
			p.next()
		}
		if p.atPunct("]") {
			p.next()
		}
	} else {
		consume()
	}
	if p.atPunct("!") {
		p.next()
	}
	return nil
}

func (p *parser) parseSelectionSet() ([]gqlSelection, error) {
	if !p.atPunct("{") {
		return nil, errors.New("expected a gqlSelection set")
	}
	p.next()
	var out []gqlSelection
	for !p.atPunct("}") && !p.at("eof") {
		if p.at("spread") {
			p.next()
			if p.atName("on") {
				// An inline fragment: keep its selections in place.
				p.next()
				if p.at("name") {
					p.next()
				}
				p.skipDirectives()
				inner, err := p.parseSelectionSet()
				if err != nil {
					return nil, err
				}
				out = append(out, inner...)
				continue
			}
			if p.at("name") {
				name := p.next().text
				p.skipDirectives()
				out = append(out, gqlSelection{name: "@fragment:" + name})
			}
			continue
		}
		sel, err := p.parseField()
		if err != nil {
			return nil, err
		}
		out = append(out, sel)
	}
	p.expect("}")
	return out, nil
}

func (p *parser) parseField() (gqlSelection, error) {
	if !p.at("name") {
		return gqlSelection{}, errors.New("expected a field name")
	}
	first := p.next().text
	sel := gqlSelection{name: first, args: map[string]any{}}
	if p.atPunct(":") {
		p.next()
		sel.aliasName = first
		if !p.at("name") {
			return gqlSelection{}, errors.New("expected a field name after an alias")
		}
		sel.name = p.next().text
	}
	if p.atPunct("(") {
		p.next()
		for !p.atPunct(")") && !p.at("eof") {
			if !p.at("name") {
				p.next()
				continue
			}
			key := p.next().text
			if p.atPunct(":") {
				p.next()
			}
			sel.args[key] = p.parseValue()
		}
		p.expect(")")
	}
	p.skipDirectives()
	if p.atPunct("{") {
		inner, err := p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
		sel.selections = inner
	}
	return sel, nil
}

// parseValue reads a literal or a variable reference.
func (p *parser) parseValue() any {
	switch {
	case p.atPunct("$"):
		p.next()
		if p.at("name") {
			return variableRef{p.next().text}
		}
		return nil
	case p.at("string"):
		return p.next().text
	case p.at("number"):
		t := p.next()
		if t.isInt {
			return t.num
		}
		return t.num
	case p.at("name"):
		// An enum value such as OPEN or DESC is a name literal; it must be
		// preserved as a string, not dropped, or filters and orderings vanish.
		switch text := p.next().text; text {
		case "true":
			return true
		case "false":
			return false
		case "null":
			return nil
		default:
			return text
		}
	case p.atPunct("["):
		p.next()
		var list []any
		for !p.atPunct("]") && !p.at("eof") {
			list = append(list, p.parseValue())
		}
		p.expect("]")
		return list
	case p.atPunct("{"):
		p.next()
		obj := map[string]any{}
		for !p.atPunct("}") && !p.at("eof") {
			if !p.at("name") {
				p.next()
				continue
			}
			key := p.next().text
			if p.atPunct(":") {
				p.next()
			}
			obj[key] = p.parseValue()
		}
		p.expect("}")
		return obj
	}
	p.next()
	return nil
}

func (p *parser) skipDirectives() {
	for p.atPunct("@") {
		p.next()
		if p.at("name") {
			p.next()
		}
		if p.atPunct("(") {
			p.next()
			depth := 1
			for depth > 0 && !p.at("eof") {
				switch {
				case p.atPunct("("):
					depth++
				case p.atPunct(")"):
					depth--
				}
				p.next()
			}
		}
	}
}

// at reports whether the parser sits on a token of the given kind. Punctuation
// is matched by atPunct instead, which compares the actual character; the
// kind-only form is used for name, number, string, spread and eof.
func (p *parser) at(kind string) bool {
	if p.pos >= len(p.toks) {
		return false
	}
	t := p.toks[p.pos]
	if kind == "eof" {
		return t.kind == "eof"
	}
	return t.kind == kind
}

// atPunct reports whether the parser sits on one specific punctuation token.
func (p *parser) atPunct(s string) bool {
	if p.pos >= len(p.toks) {
		return false
	}
	t := p.toks[p.pos]
	return t.kind == "punct" && t.text == s
}

func (p *parser) atName(name string) bool {
	return p.pos < len(p.toks) && p.toks[p.pos].kind == "name" && p.toks[p.pos].text == name
}

func (p *parser) next() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *parser) expect(punct string) {
	if p.atPunct(punct) {
		p.next()
	}
}

// variableRef marks a value that must be read from the request variables.
type variableRef struct{ name string }

// unmarshalJSON is json.Unmarshal without pulling encoding/json into callers.
func unmarshalJSON(data []byte, v any) error { return json.Unmarshal(data, v) }
