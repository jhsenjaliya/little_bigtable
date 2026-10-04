package gsql

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

type tokKind int

const (
	tEOF    tokKind = iota
	tIdent          // unquoted identifier or keyword
	tQIdent         // backquoted identifier
	tString         // string literal (decoded in str)
	tBytes          // bytes literal (decoded in b)
	tInt            // integer literal (text in text)
	tFloat          // floating point literal
	tParam          // @name
	tHint           // @{
	tOp             // operator / punctuation
)

type token struct {
	kind tokKind
	text string // raw text (operators, identifiers, number literals)
	str  string // decoded identifier / string literal
	b    []byte // decoded bytes literal
	pos  int    // byte offset in the source
}

// reservedKeywords are GoogleSQL reserved words that cannot be used as
// unquoted identifiers.
var reservedKeywords = map[string]bool{
	"ALL": true, "AND": true, "ANY": true, "ARRAY": true, "AS": true, "ASC": true,
	"ASSERT_ROWS_MODIFIED": true, "AT": true, "BETWEEN": true, "BY": true, "CASE": true,
	"CAST": true, "COLLATE": true, "CONTAINS": true, "CREATE": true, "CROSS": true,
	"CUBE": true, "CURRENT": true, "DEFAULT": true, "DEFINE": true, "DESC": true,
	"DISTINCT": true, "ELSE": true, "END": true, "ENUM": true, "ESCAPE": true,
	"EXCEPT": true, "EXCLUDE": true, "EXISTS": true, "EXTRACT": true, "FALSE": true,
	"FETCH": true, "FOLLOWING": true, "FOR": true, "FROM": true, "FULL": true,
	"GROUP": true, "GROUPING": true, "GROUPS": true, "HASH": true, "HAVING": true,
	"IF": true, "IGNORE": true, "IN": true, "INNER": true, "INTERSECT": true,
	"INTERVAL": true, "INTO": true, "IS": true, "JOIN": true, "LATERAL": true,
	"LEFT": true, "LIKE": true, "LIMIT": true, "LOOKUP": true, "MERGE": true,
	"NATURAL": true, "NEW": true, "NO": true, "NOT": true, "NULL": true, "NULLS": true,
	"OF": true, "ON": true, "OR": true, "ORDER": true, "OUTER": true, "OVER": true,
	"PARTITION": true, "PRECEDING": true, "PROTO": true, "QUALIFY": true, "RANGE": true,
	"RECURSIVE": true, "RESPECT": true, "RIGHT": true, "ROLLUP": true, "ROWS": true,
	"SELECT": true, "SET": true, "SOME": true, "STRUCT": true, "TABLESAMPLE": true,
	"THEN": true, "TO": true, "TREAT": true, "TRUE": true, "UNBOUNDED": true,
	"UNION": true, "UNNEST": true, "USING": true, "WHEN": true, "WHERE": true,
	"WINDOW": true, "WITH": true, "WITHIN": true,
}

func isReserved(word string) bool { return reservedKeywords[strings.ToUpper(word)] }

type lexer struct {
	src  string
	pos  int
	toks []token
}

func lex(src string) ([]token, error) {
	lx := &lexer{src: src}
	for {
		tok, err := lx.next()
		if err != nil {
			return nil, err
		}
		lx.toks = append(lx.toks, tok)
		if tok.kind == tEOF {
			return lx.toks, nil
		}
	}
}

// position renders a 1-based line:column for error messages.
func position(src string, off int) string {
	line, col := 1, 1
	for i := 0; i < off && i < len(src); i++ {
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return strconv.Itoa(line) + ":" + strconv.Itoa(col)
}

func (lx *lexer) errorf(off int, format string, a ...any) error {
	return invalidf("Syntax error: "+format+" [at "+position(lx.src, off)+"]", a...)
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func (lx *lexer) skipSpaceAndComments() error {
	for lx.pos < len(lx.src) {
		c := lx.src[lx.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			lx.pos++
		case c == '#' || (c == '-' && strings.HasPrefix(lx.src[lx.pos:], "--")):
			for lx.pos < len(lx.src) && lx.src[lx.pos] != '\n' {
				lx.pos++
			}
		case c == '/' && strings.HasPrefix(lx.src[lx.pos:], "/*"):
			end := strings.Index(lx.src[lx.pos+2:], "*/")
			if end < 0 {
				return lx.errorf(lx.pos, "unclosed comment")
			}
			lx.pos += end + 4
		default:
			return nil
		}
	}
	return nil
}

var multiCharOps = []string{"=>", "<=", ">=", "<>", "!=", "||", "<<", ">>", "->"}

func (lx *lexer) next() (token, error) {
	if err := lx.skipSpaceAndComments(); err != nil {
		return token{}, err
	}
	start := lx.pos
	if lx.pos >= len(lx.src) {
		return token{kind: tEOF, pos: start}, nil
	}
	c := lx.src[lx.pos]

	// String / bytes literals with optional r/b prefixes.
	if c == 'r' || c == 'R' || c == 'b' || c == 'B' {
		raw, isBytes, j := false, false, lx.pos
		lower := strings.ToLower(lx.src[lx.pos:min(lx.pos+2, len(lx.src))])
		switch {
		case lower == "rb" || lower == "br":
			raw, isBytes, j = true, true, lx.pos+2
		case lower != "" && lower[0] == 'r':
			raw, j = true, lx.pos+1
		default:
			isBytes, j = true, lx.pos+1
		}
		if j < len(lx.src) && (lx.src[j] == '\'' || lx.src[j] == '"') {
			lx.pos = j
			s, err := lx.lexQuoted(raw, isBytes, start)
			if err != nil {
				return token{}, err
			}
			if isBytes {
				return token{kind: tBytes, b: []byte(s), pos: start}, nil
			}
			return token{kind: tString, str: s, pos: start}, nil
		}
	}
	switch {
	case c == '\'' || c == '"':
		s, err := lx.lexQuoted(false, false, start)
		if err != nil {
			return token{}, err
		}
		return token{kind: tString, str: s, pos: start}, nil
	case c == '`':
		return lx.lexBackquoted()
	case isIdentStart(c) || (c == '$' && lx.pos+1 < len(lx.src) && isIdentChar(lx.src[lx.pos+1])):
		// '$colN' names anonymous SELECT columns (e.g. GROUP BY $col1).
		lx.pos++
		for lx.pos < len(lx.src) && isIdentChar(lx.src[lx.pos]) {
			lx.pos++
		}
		text := lx.src[start:lx.pos]
		return token{kind: tIdent, text: text, str: text, pos: start}, nil
	case isDigit(c) || (c == '.' && lx.pos+1 < len(lx.src) && isDigit(lx.src[lx.pos+1])):
		return lx.lexNumber()
	case c == '@':
		if lx.pos+1 < len(lx.src) && lx.src[lx.pos+1] == '{' {
			lx.pos += 2
			return token{kind: tHint, text: "@{", pos: start}, nil
		}
		lx.pos++
		if lx.pos < len(lx.src) && lx.src[lx.pos] == '`' {
			t, err := lx.lexBackquoted()
			if err != nil {
				return token{}, err
			}
			return token{kind: tParam, str: t.str, text: "@" + t.str, pos: start}, nil
		}
		if lx.pos >= len(lx.src) || !isIdentStart(lx.src[lx.pos]) {
			return token{}, lx.errorf(start, "expected parameter name after @")
		}
		s := lx.pos
		for lx.pos < len(lx.src) && isIdentChar(lx.src[lx.pos]) {
			lx.pos++
		}
		return token{kind: tParam, str: lx.src[s:lx.pos], text: lx.src[start:lx.pos], pos: start}, nil
	}
	for _, op := range multiCharOps {
		if strings.HasPrefix(lx.src[lx.pos:], op) {
			lx.pos += len(op)
			return token{kind: tOp, text: op, pos: start}, nil
		}
	}
	if strings.ContainsRune("+-*/%=<>()[],.;~&|^:{}?!", rune(c)) {
		lx.pos++
		return token{kind: tOp, text: string(c), pos: start}, nil
	}
	r, _ := utf8.DecodeRuneInString(lx.src[lx.pos:])
	return token{}, lx.errorf(start, "illegal input character %q", r)
}

func (lx *lexer) lexBackquoted() (token, error) {
	start := lx.pos
	lx.pos++ // opening `
	var sb strings.Builder
	for {
		if lx.pos >= len(lx.src) {
			return token{}, lx.errorf(start, "unclosed identifier literal")
		}
		c := lx.src[lx.pos]
		if c == '`' {
			lx.pos++
			break
		}
		if c == '\\' {
			r, n, err := lx.unescape(lx.pos, false)
			if err != nil {
				return token{}, err
			}
			sb.WriteString(r)
			lx.pos += n
			continue
		}
		sb.WriteByte(c)
		lx.pos++
	}
	if sb.Len() == 0 {
		return token{}, lx.errorf(start, "identifiers must not be empty")
	}
	return token{kind: tQIdent, text: lx.src[start:lx.pos], str: sb.String(), pos: start}, nil
}

func (lx *lexer) lexQuoted(raw, isBytes bool, start int) (string, error) {
	q := lx.src[lx.pos]
	triple := strings.HasPrefix(lx.src[lx.pos:], strings.Repeat(string(q), 3))
	if triple {
		lx.pos += 3
	} else {
		lx.pos++
	}
	var sb strings.Builder
	for {
		if lx.pos >= len(lx.src) {
			return "", lx.errorf(start, "unclosed string literal")
		}
		c := lx.src[lx.pos]
		if triple {
			if strings.HasPrefix(lx.src[lx.pos:], strings.Repeat(string(q), 3)) {
				lx.pos += 3
				break
			}
		} else {
			if c == q {
				lx.pos++
				break
			}
			if c == '\n' {
				return "", lx.errorf(start, "unclosed string literal")
			}
		}
		if c == '\\' {
			if raw {
				// In raw literals a backslash still escapes the quote character
				// (both characters are kept).
				if lx.pos+1 < len(lx.src) {
					sb.WriteByte(c)
					sb.WriteByte(lx.src[lx.pos+1])
					lx.pos += 2
					continue
				}
				return "", lx.errorf(start, "unclosed string literal")
			}
			r, n, err := lx.unescape(lx.pos, isBytes)
			if err != nil {
				return "", err
			}
			sb.WriteString(r)
			lx.pos += n
			continue
		}
		sb.WriteByte(c)
		lx.pos++
	}
	s := sb.String()
	if !isBytes && !utf8.ValidString(s) {
		return "", lx.errorf(start, "string literal is not valid UTF-8")
	}
	return s, nil
}

// unescape decodes the escape sequence at off and returns the decoded text and
// the number of source bytes consumed.
func (lx *lexer) unescape(off int, isBytes bool) (string, int, error) {
	if off+1 >= len(lx.src) {
		return "", 0, lx.errorf(off, "illegal escape sequence")
	}
	c := lx.src[off+1]
	switch c {
	case 'a':
		return "\a", 2, nil
	case 'b':
		return "\b", 2, nil
	case 'f':
		return "\f", 2, nil
	case 'n':
		return "\n", 2, nil
	case 'r':
		return "\r", 2, nil
	case 't':
		return "\t", 2, nil
	case 'v':
		return "\v", 2, nil
	case '\\', '?', '"', '\'', '`':
		return string(c), 2, nil
	case 'x', 'X':
		if off+4 <= len(lx.src) && isHex(lx.src[off+2]) && isHex(lx.src[off+3]) {
			// \x produces a single byte; string literals are validated as
			// UTF-8 once fully decoded.
			v, _ := strconv.ParseUint(lx.src[off+2:off+4], 16, 8)
			return string([]byte{byte(v)}), 4, nil
		}
		return "", 0, lx.errorf(off, "illegal \\x escape sequence")
	case 'u', 'U':
		n := 4
		if c == 'U' {
			n = 8
		}
		if off+2+n > len(lx.src) {
			return "", 0, lx.errorf(off, "illegal \\%c escape sequence", c)
		}
		v, err := strconv.ParseUint(lx.src[off+2:off+2+n], 16, 32)
		if err != nil || v > utf8.MaxRune || (v >= 0xD800 && v <= 0xDFFF) {
			return "", 0, lx.errorf(off, "illegal \\%c escape sequence", c)
		}
		return string(rune(v)), 2 + n, nil
	}
	if c >= '0' && c <= '7' {
		if off+4 <= len(lx.src) {
			o := lx.src[off+1 : off+4]
			if v, err := strconv.ParseUint(o, 8, 16); err == nil && v <= 0xff {
				return string([]byte{byte(v)}), 4, nil
			}
		}
		return "", 0, lx.errorf(off, "illegal octal escape sequence")
	}
	return "", 0, lx.errorf(off, "illegal escape sequence: \\%c", c)
}

func (lx *lexer) lexNumber() (token, error) {
	start := lx.pos
	src := lx.src
	if src[lx.pos] == '0' && lx.pos+1 < len(src) && (src[lx.pos+1] == 'x' || src[lx.pos+1] == 'X') {
		lx.pos += 2
		h := lx.pos
		for lx.pos < len(src) && isHex(src[lx.pos]) {
			lx.pos++
		}
		if lx.pos == h {
			return token{}, lx.errorf(start, "invalid hex literal")
		}
		if lx.pos < len(src) && isIdentChar(src[lx.pos]) {
			return token{}, lx.errorf(start, "Missing whitespace between literal and alias")
		}
		return token{kind: tInt, text: src[start:lx.pos], pos: start}, nil
	}
	isFloat := false
	for lx.pos < len(src) && isDigit(src[lx.pos]) {
		lx.pos++
	}
	if lx.pos < len(src) && src[lx.pos] == '.' {
		isFloat = true
		lx.pos++
		for lx.pos < len(src) && isDigit(src[lx.pos]) {
			lx.pos++
		}
	}
	if lx.pos < len(src) && (src[lx.pos] == 'e' || src[lx.pos] == 'E') {
		j := lx.pos + 1
		if j < len(src) && (src[j] == '+' || src[j] == '-') {
			j++
		}
		if j < len(src) && isDigit(src[j]) {
			isFloat = true
			lx.pos = j
			for lx.pos < len(src) && isDigit(src[lx.pos]) {
				lx.pos++
			}
		}
	}
	if lx.pos < len(src) && isIdentChar(src[lx.pos]) {
		return token{}, lx.errorf(start, "Missing whitespace between literal and alias")
	}
	if isFloat {
		return token{kind: tFloat, text: src[start:lx.pos], pos: start}, nil
	}
	return token{kind: tInt, text: src[start:lx.pos], pos: start}, nil
}
