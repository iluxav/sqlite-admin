package sqliteadmin

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Value is a SQLite value with its storage class preserved, so it can travel
// to the browser and back into a query parameter without changing type.
type Value struct {
	Kind byte // 'n' NULL, 'i' INTEGER, 'r' REAL, 't' TEXT, 'b' BLOB
	I    int64
	R    float64
	S    string // TEXT, or the raw bytes of a BLOB
}

var nullValue = Value{Kind: 'n'}

func textValue(s string) Value { return Value{Kind: 't', S: s} }

func valueFrom(v any) Value {
	switch x := v.(type) {
	case nil:
		return nullValue
	case int64:
		return Value{Kind: 'i', I: x}
	case int:
		return Value{Kind: 'i', I: int64(x)}
	case int32:
		return Value{Kind: 'i', I: int64(x)}
	case float64:
		return Value{Kind: 'r', R: x}
	case float32:
		return Value{Kind: 'r', R: float64(x)}
	case bool:
		if x {
			return Value{Kind: 'i', I: 1}
		}
		return Value{Kind: 'i', I: 0}
	case string:
		return textValue(x)
	case []byte:
		return Value{Kind: 'b', S: string(x)}
	case time.Time:
		return textValue(x.Format(time.RFC3339Nano))
	default:
		return textValue(fmt.Sprint(x))
	}
}

// Arg returns the value as a database/sql query argument.
func (v Value) Arg() any {
	switch v.Kind {
	case 'i':
		return v.I
	case 'r':
		return v.R
	case 't':
		return v.S
	case 'b':
		return []byte(v.S)
	default:
		return nil
	}
}

func (v Value) IsNull() bool { return v.Kind == 'n' }
func (v Value) IsBlob() bool { return v.Kind == 'b' }

func (v Value) raw() string {
	switch v.Kind {
	case 'i':
		return strconv.FormatInt(v.I, 10)
	case 'r':
		return strconv.FormatFloat(v.R, 'g', -1, 64)
	case 't', 'b':
		return v.S
	default:
		return ""
	}
}

func (v Value) equal(o Value) bool { return v.Kind == o.Kind && v.raw() == o.raw() }

// Literal renders the value as a SQL literal, for previews.
func (v Value) Literal() string {
	switch v.Kind {
	case 'i':
		return strconv.FormatInt(v.I, 10)
	case 'r':
		s := strconv.FormatFloat(v.R, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eEn") { // keep it REAL: 1 -> 1.0 (n: NaN/Inf)
			s += ".0"
		}
		return s
	case 't':
		return quoteString(v.S)
	case 'b':
		return "x'" + hex.EncodeToString([]byte(v.S)) + "'"
	default:
		return "NULL"
	}
}

// Display is the full text representation shown to the user.
func (v Value) Display() string {
	switch v.Kind {
	case 'n':
		return "NULL"
	case 'b':
		return fmt.Sprintf("BLOB · %d bytes", len(v.S))
	case 't':
		if !utf8.ValidString(v.S) {
			return strings.ToValidUTF8(v.S, "�")
		}
		return v.S
	default:
		return v.raw()
	}
}

// Short is Display truncated for grid cells.
func (v Value) Short() string {
	s := v.Display()
	const max = 120
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max]) + "…"
}

// EditText is the text placed in an editor input.
func (v Value) EditText() string {
	if v.Kind == 'n' || v.Kind == 'b' {
		return ""
	}
	return v.Display()
}

// TypeName is the SQLite storage class name.
func (v Value) TypeName() string {
	switch v.Kind {
	case 'i':
		return "integer"
	case 'r':
		return "real"
	case 't':
		return "text"
	case 'b':
		return "blob"
	default:
		return "null"
	}
}

// encodeValues encodes values as an opaque, URL-safe token ("i:NQ.t:YWJj")
// that preserves storage classes and arbitrary bytes.
func encodeValues(vs []Value) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = string(v.Kind) + ":" + base64.RawURLEncoding.EncodeToString([]byte(v.raw()))
	}
	return strings.Join(parts, ".")
}

func decodeValues(s string) ([]Value, error) {
	if s == "" {
		return nil, errors.New("empty row key")
	}
	parts := strings.Split(s, ".")
	out := make([]Value, len(parts))
	for i, p := range parts {
		if len(p) < 2 || p[1] != ':' {
			return nil, errors.New("malformed row key")
		}
		b, err := base64.RawURLEncoding.DecodeString(p[2:])
		if err != nil {
			return nil, errors.New("malformed row key")
		}
		raw := string(b)
		switch k := p[0]; k {
		case 'n':
			out[i] = nullValue
		case 'i':
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, errors.New("malformed row key")
			}
			out[i] = Value{Kind: 'i', I: n}
		case 'r':
			f, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, errors.New("malformed row key")
			}
			out[i] = Value{Kind: 'r', R: f}
		case 't', 'b':
			out[i] = Value{Kind: k, S: raw}
		default:
			return nil, errors.New("malformed row key")
		}
	}
	return out, nil
}

// paramFor converts text typed by the user into the parameter bound for col.
// Columns with a type affinity get text and SQLite converts it; columns
// without one (BLOB/ANY/untyped) would store text verbatim, so numeric-looking
// input is bound as a number instead.
func paramFor(col Column, text string) Value {
	if col.affinity() != "BLOB" {
		return textValue(text)
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return Value{Kind: 'i', I: n}
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil && strings.ContainsAny(text, ".eE") {
		return Value{Kind: 'r', R: f}
	}
	return textValue(text)
}
