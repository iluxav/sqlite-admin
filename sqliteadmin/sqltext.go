package sqliteadmin

import (
	"errors"
	"strings"
)

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func quoteString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

type tokKind int

const (
	tkWord    tokKind = iota // keywords, bare identifiers, numbers
	tkString                 // '...'
	tkIdent                  // "...", `...`, [...]
	tkPunct                  // any other single character
	tkSpace                  // whitespace
	tkComment                // -- line or /* block */
)

type token struct {
	kind tokKind
	text string
	pos  int
}

func (t token) significant() bool { return t.kind != tkSpace && t.kind != tkComment }

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// tokenize is a minimal SQLite lexer: just enough to find statement
// boundaries, parentheses and identifiers outside strings and comments.
func tokenize(s string) []token {
	var out []token
	for i := 0; i < len(s); {
		start := i
		c := s[i]
		kind := tkPunct
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			for i < len(s) && strings.IndexByte(" \t\n\r\f", s[i]) >= 0 {
				i++
			}
			kind = tkSpace
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			kind = tkComment
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				i = len(s)
			} else {
				i += 2 + end + 2
			}
			kind = tkComment
		case c == '\'' || c == '"' || c == '`':
			i = scanQuoted(s, i, c)
			kind = tkIdent
			if c == '\'' {
				kind = tkString
			}
		case c == '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				i = len(s)
			} else {
				i += end + 1
			}
			kind = tkIdent
		case isWordByte(c):
			for i < len(s) && isWordByte(s[i]) {
				i++
			}
			kind = tkWord
		default:
			i++
		}
		out = append(out, token{kind: kind, text: s[start:i], pos: start})
	}
	return out
}

// scanQuoted returns the index just past a quoted run starting at i, where a
// doubled quote character is an escaped quote.
func scanQuoted(s string, i int, q byte) int {
	i++
	for i < len(s) {
		if s[i] == q {
			if i+1 < len(s) && s[i+1] == q {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

// unquoteIdent returns the identifier named by a token.
func unquoteIdent(t token) string {
	switch t.kind {
	case tkIdent, tkString:
		if t.text[0] == '[' {
			return strings.TrimSuffix(t.text[1:], "]")
		}
		q := t.text[:1]
		inner := strings.TrimPrefix(t.text, q)
		inner = strings.TrimSuffix(inner, q)
		return strings.ReplaceAll(inner, q+q, q)
	default:
		return t.text
	}
}

// leadingWords returns up to n leading keyword tokens, upper-cased.
func leadingWords(s string, n int) []string {
	var out []string
	for _, t := range tokenize(s) {
		if !t.significant() {
			continue
		}
		if t.kind != tkWord || len(out) == n {
			break
		}
		out = append(out, strings.ToUpper(t.text))
	}
	return out
}

// splitStatements splits SQL text into statements, respecting strings,
// quoted identifiers, comments, and semicolons inside CREATE TRIGGER bodies.
func splitStatements(s string) []string {
	var out []string
	start := 0
	var words []string
	trigger, sawBegin, nonEmpty := false, false, false
	depth := 0
	flush := func(end int) {
		if nonEmpty {
			out = append(out, strings.TrimSpace(s[start:end]))
		}
		words, trigger, sawBegin, nonEmpty, depth = nil, false, false, false, 0
	}
	for _, t := range tokenize(s) {
		if !t.significant() {
			continue
		}
		if t.kind == tkPunct && t.text == ";" {
			if !trigger || (sawBegin && depth <= 0) {
				flush(t.pos)
				start = t.pos + 1
			}
			continue
		}
		nonEmpty = true
		if t.kind != tkWord {
			continue
		}
		w := strings.ToUpper(t.text)
		if len(words) < 3 {
			words = append(words, w)
			trigger = trigger || isCreateTrigger(words)
		}
		if trigger {
			switch w {
			case "BEGIN":
				depth++
				sawBegin = true
			case "CASE":
				depth++
			case "END":
				depth--
			}
		}
	}
	flush(len(s))
	return out
}

func isCreateTrigger(w []string) bool {
	if len(w) < 2 || w[0] != "CREATE" {
		return false
	}
	return w[1] == "TRIGGER" || (len(w) > 2 && (w[1] == "TEMP" || w[1] == "TEMPORARY") && w[2] == "TRIGGER")
}

// checkFragment rejects user-supplied SQL fragments (filters, defaults)
// that would smuggle in an additional statement.
func checkFragment(what, s string) error {
	for _, t := range tokenize(s) {
		if t.kind == tkPunct && t.text == ";" {
			return errors.New(what + " must not contain ';'")
		}
	}
	return nil
}

// createTable is a CREATE TABLE statement split into its top-level parts.
type createTable struct {
	items  []string // column definitions and table constraints
	suffix string   // table options after the closing parenthesis
}

func parseCreateTable(s string) (*createTable, error) {
	w := leadingWords(s, 2)
	if len(w) < 2 || w[0] != "CREATE" || w[1] != "TABLE" {
		return nil, errors.New("not a plain CREATE TABLE statement")
	}
	depth, itemStart := 0, -1
	ct := &createTable{}
	for _, t := range tokenize(s) {
		if t.kind != tkPunct {
			continue
		}
		switch t.text {
		case "(":
			if depth == 0 {
				itemStart = t.pos + 1
			}
			depth++
		case ",":
			if depth == 1 {
				ct.items = append(ct.items, strings.TrimSpace(s[itemStart:t.pos]))
				itemStart = t.pos + 1
			}
		case ")":
			depth--
			if depth == 0 {
				ct.items = append(ct.items, strings.TrimSpace(s[itemStart:t.pos]))
				ct.suffix = strings.TrimSpace(s[t.pos+1:])
				return ct, nil
			}
		}
	}
	return nil, errors.New("could not parse CREATE TABLE statement")
}

var constraintWords = map[string]bool{"CONSTRAINT": true, "PRIMARY": true, "UNIQUE": true, "CHECK": true, "FOREIGN": true}

// itemColumn returns the column name defined by a CREATE TABLE item, or
// ok=false for table constraints.
func itemColumn(item string) (name string, ok bool) {
	for _, t := range tokenize(item) {
		if !t.significant() {
			continue
		}
		if t.kind == tkWord && constraintWords[strings.ToUpper(t.text)] {
			return "", false
		}
		return unquoteIdent(t), true
	}
	return "", false
}

// itemGenerated reports whether a column definition is a generated column
// (it has a top-level AS clause).
func itemGenerated(item string) bool {
	depth := 0
	for _, t := range tokenize(item) {
		switch {
		case t.kind == tkPunct && t.text == "(":
			depth++
		case t.kind == tkPunct && t.text == ")":
			depth--
		case depth == 0 && t.kind == tkWord && strings.EqualFold(t.text, "AS"):
			return true
		}
	}
	return false
}

// endsWithLineComment reports whether s ends inside a "--" comment, so that
// whatever is appended after it must start on a new line.
func endsWithLineComment(s string) bool {
	toks := tokenize(s)
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].kind == tkSpace {
			continue
		}
		return toks[i].kind == tkComment && strings.HasPrefix(toks[i].text, "--")
	}
	return false
}

// build renders the statement as CREATE TABLE <name> (...) <suffix>.
func (ct *createTable) build(name string) string {
	var b strings.Builder
	b.WriteString("CREATE TABLE " + quoteIdent(name) + " (\n")
	for i, item := range ct.items {
		b.WriteString("  " + item)
		if endsWithLineComment(item) {
			b.WriteString("\n")
		}
		if i < len(ct.items)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(")")
	if ct.suffix != "" {
		b.WriteString(" " + ct.suffix)
	}
	return b.String()
}

func containsWord(s, word string) bool {
	for _, t := range tokenize(s) {
		if t.kind == tkWord && strings.EqualFold(t.text, word) {
			return true
		}
	}
	return false
}
