package gsql

import (
	"math"
	"strconv"
	"strings"
)

type parser struct {
	src   string
	toks  []token
	i     int
	depth int
}

// parseStatement parses a single query statement.
func parseStatement(src string) (*astQuery, []astNamedArg, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, nil, err
	}
	p := &parser{src: src, toks: toks}
	var hints []astNamedArg
	if p.peek().kind == tHint {
		if hints, err = p.parseHints(); err != nil {
			return nil, nil, err
		}
	}
	t := p.peek()
	switch {
	case p.isKw(t, "SELECT") || p.isOp(t, "("):
	case p.isKw(t, "WITH"):
		return nil, nil, invalidf("WITH clauses (common table expressions) are not supported by GoogleSQL for Bigtable")
	case t.kind == tIdent && isStatementKeyword(t.text):
		return nil, nil, invalidf("%s statements are not supported; GoogleSQL for Bigtable only supports SELECT queries", strings.ToUpper(t.text))
	case t.kind == tEOF:
		return nil, nil, invalidf("Syntax error: empty query")
	default:
		return nil, nil, p.errorf(t, "expected SELECT but got %s", p.describe(t))
	}
	q, err := p.parseQuery()
	if err != nil {
		return nil, nil, err
	}
	p.acceptOp(";")
	if t := p.peek(); t.kind != tEOF {
		return nil, nil, p.errorf(t, "unexpected %s", p.describe(t))
	}
	return q, hints, nil
}

func isStatementKeyword(w string) bool {
	switch strings.ToUpper(w) {
	case "INSERT", "UPDATE", "DELETE", "MERGE", "CREATE", "ALTER", "DROP", "GRANT",
		"REVOKE", "TRUNCATE", "REPLACE", "UPSERT", "CALL", "EXPLAIN", "DESCRIBE",
		"SHOW", "BEGIN", "COMMIT", "ROLLBACK", "SET", "DEFINE", "EXPORT", "IMPORT":
		return true
	}
	return false
}

func (p *parser) peek() token { return p.toks[p.i] }

func (p *parser) peekN(n int) token {
	if p.i+n < len(p.toks) {
		return p.toks[p.i+n]
	}
	return p.toks[len(p.toks)-1]
}

func (p *parser) advance() token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *parser) isKw(t token, kw string) bool {
	return t.kind == tIdent && strings.EqualFold(t.text, kw)
}

func (p *parser) isOp(t token, op string) bool { return t.kind == tOp && t.text == op }

func (p *parser) acceptKw(kw string) bool {
	if p.isKw(p.peek(), kw) {
		p.advance()
		return true
	}
	return false
}

func (p *parser) acceptOp(op string) bool {
	if p.isOp(p.peek(), op) {
		p.advance()
		return true
	}
	return false
}

func (p *parser) expectKw(kw string) error {
	if !p.acceptKw(kw) {
		return p.errorf(p.peek(), "expected keyword %s but got %s", kw, p.describe(p.peek()))
	}
	return nil
}

func (p *parser) expectOp(op string) error {
	if !p.acceptOp(op) {
		return p.errorf(p.peek(), "expected %q but got %s", op, p.describe(p.peek()))
	}
	return nil
}

// expectGT consumes a '>' closing a type parameter list, splitting '>>'.
func (p *parser) expectGT() error {
	t := p.peek()
	if p.isOp(t, ">") {
		p.advance()
		return nil
	}
	if p.isOp(t, ">>") {
		p.toks[p.i].text = ">"
		p.toks[p.i].pos++
		return nil
	}
	if p.isOp(t, ">=") {
		p.toks[p.i].text = "="
		p.toks[p.i].pos++
		return nil
	}
	return p.errorf(t, "expected \">\" but got %s", p.describe(t))
}

func (p *parser) describe(t token) string {
	switch t.kind {
	case tEOF:
		return "end of input"
	case tIdent:
		if isReserved(t.text) {
			return "keyword " + strings.ToUpper(t.text)
		}
		return "identifier " + t.text
	case tQIdent:
		return "identifier " + t.text
	case tString:
		return "string literal"
	case tBytes:
		return "bytes literal"
	case tInt, tFloat:
		return "literal " + t.text
	case tParam:
		return "query parameter " + t.text
	case tHint:
		return "hint"
	}
	return "\"" + t.text + "\""
}

func (p *parser) errorf(t token, format string, a ...any) error {
	return invalidf("Syntax error: "+format+" [at "+position(p.src, t.pos)+"]", a...)
}

// identifier accepts an unquoted non-reserved identifier or a quoted one.
func (p *parser) identifier(what string) (string, error) {
	t := p.peek()
	switch {
	case t.kind == tQIdent:
		p.advance()
		return t.str, nil
	case t.kind == tIdent && !isReserved(t.text):
		p.advance()
		return t.str, nil
	}
	return "", p.errorf(t, "expected %s but got %s", what, p.describe(t))
}

func (p *parser) parseHints() ([]astNamedArg, error) {
	start := p.advance() // @{
	var hints []astNamedArg
	for {
		t := p.peek()
		if t.kind != tIdent && t.kind != tQIdent {
			return nil, p.errorf(t, "expected hint name")
		}
		p.advance()
		name := t.str
		for p.acceptOp(".") {
			n := p.advance()
			name += "." + n.str
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		hints = append(hints, astNamedArg{name: name, x: x, pos: t.pos})
		if p.acceptOp(",") {
			continue
		}
		if err := p.expectOp("}"); err != nil {
			return nil, err
		}
		_ = start
		return hints, nil
	}
}

func (p *parser) parseQuery() (*astQuery, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 64 {
		return nil, p.errorf(p.peek(), "query is nested too deeply")
	}
	q := &astQuery{pos: p.peek().pos}
	if p.acceptOp("(") {
		inner, err := p.parseQuery()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		q.inner = inner
	} else {
		sel, err := p.parseSelect()
		if err != nil {
			return nil, err
		}
		q.sel = sel
	}
	if t := p.peek(); p.isKw(t, "UNION") || p.isKw(t, "INTERSECT") || p.isKw(t, "EXCEPT") {
		return nil, invalidf("set operations (UNION, INTERSECT, EXCEPT) are not supported by GoogleSQL for Bigtable [at %s]", position(p.src, t.pos))
	}
	if p.acceptKw("ORDER") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		items, err := p.parseOrderItems()
		if err != nil {
			return nil, err
		}
		q.orderBy = items
	}
	if p.acceptKw("LIMIT") {
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		q.limit = x
		if p.acceptKw("OFFSET") {
			o, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			q.offset = o
		}
	} else if t := p.peek(); p.isKw(t, "OFFSET") {
		return nil, p.errorf(t, "OFFSET requires a LIMIT clause")
	}
	return q, nil
}

func (p *parser) parseOrderItems() ([]astOrderItem, error) {
	var items []astOrderItem
	for {
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		it := astOrderItem{x: x}
		if p.acceptKw("ASC") {
			it.explicit = true
		} else if p.acceptKw("DESC") {
			it.desc, it.explicit = true, true
		}
		if p.acceptKw("NULLS") {
			it.nullsSeen = true
			switch {
			case p.acceptKw("FIRST"):
				it.nulls = "FIRST"
			case p.acceptKw("LAST"):
				it.nulls = "LAST"
			default:
				return nil, p.errorf(p.peek(), "expected FIRST or LAST after NULLS")
			}
		}
		items = append(items, it)
		if !p.acceptOp(",") {
			return items, nil
		}
	}
}

func (p *parser) parseSelect() (*astSelect, error) {
	start := p.peek()
	if err := p.expectKw("SELECT"); err != nil {
		return nil, err
	}
	sel := &astSelect{pos: start.pos}
	if p.isKw(p.peek(), "AS") {
		t := p.peekN(1)
		if p.isKw(t, "STRUCT") || p.isKw(t, "VALUE") {
			return nil, unimplementedf("SELECT AS %s is not supported by the emulator", strings.ToUpper(t.text))
		}
	}
	if p.acceptKw("DISTINCT") {
		sel.distinct = true
	} else {
		p.acceptKw("ALL")
	}
	for {
		t := p.peek()
		if t.kind == tEOF || p.isKw(t, "FROM") || p.isOp(t, ")") || p.isOp(t, ";") ||
			p.isKw(t, "WHERE") || p.isKw(t, "GROUP") || p.isKw(t, "ORDER") || p.isKw(t, "LIMIT") || p.isKw(t, "HAVING") {
			if len(sel.items) == 0 {
				return nil, p.errorf(t, "SELECT list must not be empty")
			}
			break
		}
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		sel.items = append(sel.items, item)
		if !p.acceptOp(",") {
			break
		}
	}
	if p.acceptKw("FROM") {
		f, err := p.parseFrom()
		if err != nil {
			return nil, err
		}
		sel.from = f
	}
	if p.acceptKw("WHERE") {
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		sel.where = x
	}
	if p.acceptKw("GROUP") {
		if err := p.expectKw("BY"); err != nil {
			return nil, err
		}
		t := p.peek()
		if p.isKw(t, "ROLLUP") || p.isKw(t, "CUBE") || p.isKw(t, "GROUPING") {
			return nil, unimplementedf("GROUP BY %s is not supported by the emulator", strings.ToUpper(t.text))
		}
		if p.isKw(t, "ALL") {
			return nil, unimplementedf("GROUP BY ALL is not supported by the emulator")
		}
		for {
			x, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			sel.groupBy = append(sel.groupBy, x)
			if !p.acceptOp(",") {
				break
			}
		}
	}
	if p.acceptKw("HAVING") {
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		sel.having = x
	}
	if t := p.peek(); p.isKw(t, "WINDOW") {
		return nil, unimplementedf("the WINDOW clause and window functions are not supported by the emulator")
	}
	if t := p.peek(); p.isKw(t, "QUALIFY") {
		return nil, unimplementedf("the QUALIFY clause is not supported by the emulator")
	}
	return sel, nil
}

func (p *parser) parseSelectItem() (astSelectItem, error) {
	start := p.peek()
	item := astSelectItem{pos: start.pos}
	if p.acceptOp("*") {
		item.star = true
		return item, p.parseStarModifiers(&item)
	}
	x, err := p.parseExpr()
	if err != nil {
		return item, err
	}
	if p.isOp(p.peek(), ".") && p.isOp(p.peekN(1), "*") {
		p.advance()
		p.advance()
		item.star = true
		item.starExpr = x
		return item, p.parseStarModifiers(&item)
	}
	item.expr = x
	item.exprSource = strings.TrimSpace(p.src[start.pos:p.peek().pos])
	if p.acceptKw("AS") {
		name, err := p.identifier("alias")
		if err != nil {
			return item, err
		}
		item.alias, item.hasAlias = name, true
	} else if t := p.peek(); t.kind == tQIdent || (t.kind == tIdent && !isReserved(t.text)) {
		p.advance()
		item.alias, item.hasAlias = t.str, true
	}
	return item, nil
}

func (p *parser) parseStarModifiers(item *astSelectItem) error {
	if p.acceptKw("EXCEPT") {
		if err := p.expectOp("("); err != nil {
			return err
		}
		for {
			name, err := p.identifier("column name")
			if err != nil {
				return err
			}
			item.except = append(item.except, name)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return err
		}
	}
	if p.acceptKw("REPLACE") {
		if err := p.expectOp("("); err != nil {
			return err
		}
		for {
			x, err := p.parseExpr()
			if err != nil {
				return err
			}
			if err := p.expectKw("AS"); err != nil {
				return err
			}
			name, err := p.identifier("column name")
			if err != nil {
				return err
			}
			item.replace = append(item.replace, astReplace{x: x, name: name})
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) parseFrom() (*astFrom, error) {
	t := p.peek()
	f := &astFrom{pos: t.pos}
	switch {
	case p.isOp(t, "("):
		return nil, invalidf("subqueries in the FROM clause are not supported by GoogleSQL for Bigtable [at %s]", position(p.src, t.pos))
	case p.isKw(t, "UNNEST"):
		return nil, unimplementedf("UNNEST in the FROM clause is not supported by the emulator")
	case t.kind == tIdent && strings.EqualFold(t.text, "UNPACK") && p.isOp(p.peekN(1), "("):
		p.advance()
		p.advance()
		var q *astQuery
		var err error
		if p.isOp(p.peek(), "(") || p.isKw(p.peek(), "SELECT") {
			q, err = p.parseQuery()
		} else {
			return nil, p.errorf(p.peek(), "UNPACK expects a subquery")
		}
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		f.unpack = q
	default:
		name, err := p.identifier("table name")
		if err != nil {
			return nil, err
		}
		if p.isOp(p.peek(), ".") {
			return nil, invalidf("Table name format not supported: use backquotes for table names containing '.' [at %s]", position(p.src, t.pos))
		}
		f.table = name
		if p.acceptOp("(") {
			f.hasArgs = true
			if !p.acceptOp(")") {
				for {
					at := p.peek()
					if (at.kind != tIdent && at.kind != tQIdent) || !p.isOp(p.peekN(1), "=>") {
						return nil, p.errorf(at, "table arguments must be named, e.g. with_history => TRUE")
					}
					p.advance()
					p.advance()
					x, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					f.tableArgs = append(f.tableArgs, astNamedArg{name: at.str, x: x, pos: at.pos})
					if p.acceptOp(",") {
						continue
					}
					if err := p.expectOp(")"); err != nil {
						return nil, err
					}
					break
				}
			}
		}
	}
	if p.peek().kind == tHint {
		h, err := p.parseHints()
		if err != nil {
			return nil, err
		}
		f.hints = h
	}
	if p.acceptKw("AS") {
		a, err := p.identifier("alias")
		if err != nil {
			return nil, err
		}
		f.alias = a
	} else if t := p.peek(); t.kind == tQIdent || (t.kind == tIdent && !isReserved(t.text)) {
		p.advance()
		f.alias = t.str
	}
	if t := p.peek(); p.isOp(t, ",") || p.isKw(t, "JOIN") || p.isKw(t, "CROSS") || p.isKw(t, "INNER") ||
		p.isKw(t, "LEFT") || p.isKw(t, "RIGHT") || p.isKw(t, "FULL") || p.isKw(t, "NATURAL") {
		return nil, invalidf("JOIN is not supported by GoogleSQL for Bigtable [at %s]", position(p.src, t.pos))
	}
	if t := p.peek(); p.isKw(t, "TABLESAMPLE") {
		return nil, unimplementedf("TABLESAMPLE is not supported by the emulator")
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// Expressions.

func (p *parser) parseExpr() (astExpr, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 200 {
		return nil, p.errorf(p.peek(), "expression is nested too deeply")
	}
	return p.parseOr()
}

func (p *parser) parseOr() (astExpr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if !p.isKw(t, "OR") {
			return l, nil
		}
		p.advance()
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &astBinary{op: "OR", l: l, r: r, pos: t.pos}
	}
}

func (p *parser) parseAnd() (astExpr, error) {
	l, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if !p.isKw(t, "AND") {
			return l, nil
		}
		p.advance()
		r, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l = &astBinary{op: "AND", l: l, r: r, pos: t.pos}
	}
}

func (p *parser) parseNot() (astExpr, error) {
	t := p.peek()
	if p.isKw(t, "NOT") {
		p.advance()
		x, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &astUnary{op: "NOT", x: x, pos: t.pos}, nil
	}
	return p.parseComparison()
}

var comparisonOps = map[string]bool{"=": true, "!=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

func (p *parser) parseComparison() (astExpr, error) {
	l, err := p.parseBitOr()
	if err != nil {
		return nil, err
	}
	t := p.peek()
	switch {
	case t.kind == tOp && comparisonOps[t.text]:
		p.advance()
		r, err := p.parseBitOr()
		if err != nil {
			return nil, err
		}
		op := t.text
		if op == "<>" {
			op = "!="
		}
		res := &astBinary{op: op, l: l, r: r, pos: t.pos}
		if n := p.peek(); n.kind == tOp && comparisonOps[n.text] {
			return nil, p.errorf(n, "comparison operators cannot be chained")
		}
		return res, nil
	case p.isKw(t, "IS"):
		p.advance()
		not := p.acceptKw("NOT")
		w := p.peek()
		switch {
		case p.isKw(w, "NULL"), p.isKw(w, "TRUE"), p.isKw(w, "FALSE"), strings.EqualFold(w.text, "UNKNOWN") && w.kind == tIdent:
			p.advance()
			what := strings.ToUpper(w.text)
			if what == "UNKNOWN" {
				what = "NULL"
			}
			return &astIs{x: l, not: not, what: what, pos: t.pos}, nil
		case p.isKw(w, "DISTINCT"):
			p.advance()
			if err := p.expectKw("FROM"); err != nil {
				return nil, err
			}
			r, err := p.parseBitOr()
			if err != nil {
				return nil, err
			}
			name := "$is_distinct_from"
			if not {
				name = "$is_not_distinct_from"
			}
			return &astCall{name: name, args: []astExpr{l, r}, pos: t.pos}, nil
		}
		return nil, p.errorf(w, "expected NULL, TRUE, FALSE or DISTINCT FROM after IS")
	}
	not := false
	if p.isKw(t, "NOT") {
		n := p.peekN(1)
		if p.isKw(n, "IN") || p.isKw(n, "LIKE") || p.isKw(n, "BETWEEN") {
			p.advance()
			not = true
			t = p.peek()
		}
	}
	switch {
	case p.isKw(t, "IN"):
		p.advance()
		in := &astIn{x: l, not: not, pos: t.pos}
		if p.acceptKw("UNNEST") {
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			x, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			in.unnest = x
			return in, nil
		}
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		if p.isKw(p.peek(), "SELECT") || p.isKw(p.peek(), "WITH") {
			return nil, invalidf("subqueries are not supported by GoogleSQL for Bigtable [at %s]", position(p.src, p.peek().pos))
		}
		for {
			x, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			in.list = append(in.list, x)
			if !p.acceptOp(",") {
				break
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		return in, nil
	case p.isKw(t, "LIKE"):
		p.advance()
		if n := p.peek(); p.isKw(n, "ANY") || p.isKw(n, "SOME") || p.isKw(n, "ALL") {
			return nil, unimplementedf("quantified LIKE (LIKE %s) is not supported by the emulator", strings.ToUpper(n.text))
		}
		r, err := p.parseBitOr()
		if err != nil {
			return nil, err
		}
		if p.isKw(p.peek(), "ESCAPE") {
			return nil, unimplementedf("LIKE ... ESCAPE is not supported by the emulator")
		}
		return &astLike{x: l, pattern: r, not: not, pos: t.pos}, nil
	case p.isKw(t, "BETWEEN"):
		p.advance()
		lo, err := p.parseBitOr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("AND"); err != nil {
			return nil, err
		}
		hi, err := p.parseBitOr()
		if err != nil {
			return nil, err
		}
		return &astBetween{x: l, lo: lo, hi: hi, not: not, pos: t.pos}, nil
	}
	return l, nil
}

func (p *parser) parseBinaryLevel(ops []string, next func() (astExpr, error)) (astExpr, error) {
	l, err := next()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		matched := ""
		if t.kind == tOp {
			for _, op := range ops {
				if t.text == op {
					matched = op
					break
				}
			}
		}
		if matched == "" {
			return l, nil
		}
		p.advance()
		r, err := next()
		if err != nil {
			return nil, err
		}
		l = &astBinary{op: matched, l: l, r: r, pos: t.pos}
	}
}

func (p *parser) parseBitOr() (astExpr, error) {
	return p.parseBinaryLevel([]string{"|"}, p.parseBitXor)
}
func (p *parser) parseBitXor() (astExpr, error) {
	return p.parseBinaryLevel([]string{"^"}, p.parseBitAnd)
}
func (p *parser) parseBitAnd() (astExpr, error) {
	return p.parseBinaryLevel([]string{"&"}, p.parseShift)
}
func (p *parser) parseShift() (astExpr, error) {
	return p.parseBinaryLevel([]string{"<<", ">>"}, p.parseAdd)
}
func (p *parser) parseAdd() (astExpr, error) {
	return p.parseBinaryLevel([]string{"+", "-"}, p.parseMul)
}
func (p *parser) parseMul() (astExpr, error) {
	return p.parseBinaryLevel([]string{"*", "/", "||"}, p.parseUnary)
}

func (p *parser) parseUnary() (astExpr, error) {
	t := p.peek()
	if p.isOp(t, "-") || p.isOp(t, "+") || p.isOp(t, "~") {
		p.advance()
		if p.isOp(t, "-") {
			n := p.peek()
			if n.kind == tInt && !p.isOp(p.peekN(1), "[") && !p.isOp(p.peekN(1), ".") {
				p.advance()
				v, err := parseIntLiteral("-" + n.text)
				if err != nil {
					return nil, p.errorf(n, "%v", err)
				}
				return &astLit{kind: litInt, val: v, pos: t.pos}, nil
			}
		}
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if p.isOp(t, "+") {
			return &astUnary{op: "+", x: x, pos: t.pos}, nil
		}
		return &astUnary{op: t.text, x: x, pos: t.pos}, nil
	}
	return p.parsePostfix()
}

func parseIntLiteral(text string) (int64, error) {
	neg := strings.HasPrefix(text, "-")
	s := strings.TrimPrefix(text, "-")
	var u uint64
	var err error
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		u, err = strconv.ParseUint(s[2:], 16, 64)
	} else {
		u, err = strconv.ParseUint(s, 10, 64)
	}
	if err != nil {
		return 0, invalidf("invalid integer literal: %s", text)
	}
	if neg {
		if u > 1<<63 {
			return 0, invalidf("invalid integer literal: %s", text)
		}
		return int64(-u), nil
	}
	if u > math.MaxInt64 {
		return 0, invalidf("invalid integer literal: %s", text)
	}
	return int64(u), nil
}

func (p *parser) parsePostfix() (astExpr, error) {
	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		switch {
		case p.isOp(t, "["):
			p.advance()
			idx := &astIndex{x: x, pos: t.pos}
			n := p.peek()
			if n.kind == tIdent && p.isOp(p.peekN(1), "(") {
				switch m := strings.ToUpper(n.text); m {
				case "OFFSET", "SAFE_OFFSET", "ORDINAL", "SAFE_ORDINAL", "KEY", "SAFE_KEY":
					p.advance()
					p.advance()
					e, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					if err := p.expectOp(")"); err != nil {
						return nil, err
					}
					idx.mode, idx.idx = m, e
				}
			}
			if idx.idx == nil {
				e, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				idx.idx = e
			}
			if err := p.expectOp("]"); err != nil {
				return nil, err
			}
			x = idx
		case p.isOp(t, "."):
			n := p.peekN(1)
			if p.isOp(n, "*") {
				return x, nil
			}
			if n.kind != tIdent && n.kind != tQIdent {
				return nil, p.errorf(n, "expected field name after \".\"")
			}
			p.advance()
			p.advance()
			x = &astField{x: x, name: n.str, pos: n.pos}
		default:
			return x, nil
		}
	}
}

// unsupportedFunctions are valid GoogleSQL for Bigtable functions that the
// emulator does not implement. Their arguments are skipped without parsing
// (some use lambda syntax).
func unsupportedFunction(name string) (string, bool) {
	switch {
	case strings.HasPrefix(name, "ST_"), strings.HasPrefix(name, "S2_"):
		return "geography function " + name, true
	}
	switch name {
	case "ROW_NUMBER", "RANK", "DENSE_RANK", "PERCENT_RANK", "CUME_DIST", "NTILE",
		"LAG", "LEAD", "FIRST_VALUE", "LAST_VALUE", "NTH_VALUE", "PERCENTILE_CONT", "PERCENTILE_DISC":
		return "window function " + name, true
	case "APPROX_QUANTILES", "APPROX_TOP_COUNT", "APPROX_TOP_SUM":
		return "approximate aggregate function " + name, true
	case "ARRAY_FILTER", "ARRAY_TRANSFORM":
		return "lambda function " + name, true
	case "NORMALIZE", "NORMALIZE_AND_CASEFOLD":
		return "Unicode normalization function " + name, true
	case "CLUSTER_ATTRIBUTE":
		return "non-deterministic function CLUSTER_ATTRIBUTE", true
	}
	return "", false
}

// skipBalanced skips a parenthesized token group starting at '('.
func (p *parser) skipBalanced() error {
	if !p.isOp(p.peek(), "(") {
		return p.errorf(p.peek(), "expected \"(\"")
	}
	depth := 0
	for {
		t := p.advance()
		switch {
		case t.kind == tEOF:
			return p.errorf(t, "unbalanced parentheses")
		case p.isOp(t, "("):
			depth++
		case p.isOp(t, ")"):
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

// skipOver skips an OVER clause if present and reports whether one was found.
func (p *parser) skipOver() (bool, error) {
	if !p.acceptKw("OVER") {
		return false, nil
	}
	if p.isOp(p.peek(), "(") {
		return true, p.skipBalanced()
	}
	if _, err := p.identifier("window name"); err != nil {
		return true, err
	}
	return true, nil
}

func (p *parser) parsePrimary() (astExpr, error) {
	t := p.peek()
	switch t.kind {
	case tInt:
		p.advance()
		v, err := parseIntLiteral(t.text)
		if err != nil {
			return nil, p.errorf(t, "%v", err)
		}
		return &astLit{kind: litInt, val: v, pos: t.pos}, nil
	case tFloat:
		p.advance()
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, p.errorf(t, "invalid floating point literal %s", t.text)
		}
		return &astLit{kind: litFloat, val: f, pos: t.pos}, nil
	case tString:
		p.advance()
		return &astLit{kind: litString, val: t.str, pos: t.pos}, nil
	case tBytes:
		p.advance()
		return &astLit{kind: litBytes, val: t.b, pos: t.pos}, nil
	case tParam:
		p.advance()
		return &astParam{name: t.str, pos: t.pos}, nil
	case tOp:
		switch t.text {
		case "(":
			p.advance()
			if n := p.peek(); p.isKw(n, "SELECT") || p.isKw(n, "WITH") {
				return nil, invalidf("subqueries are not supported by GoogleSQL for Bigtable [at %s]", position(p.src, n.pos))
			}
			x, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if p.acceptOp(",") {
				st := &astStruct{tuple: true, fields: []astExpr{x}, names: []string{""}, pos: t.pos}
				for {
					y, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					st.fields = append(st.fields, y)
					st.names = append(st.names, "")
					if !p.acceptOp(",") {
						break
					}
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				return st, nil
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return x, nil
		case "[":
			return p.parseArrayLiteral(nil, t.pos)
		}
		return nil, p.errorf(t, "unexpected %s", p.describe(t))
	case tQIdent:
		return p.parseIdentOrCall()
	case tIdent:
		up := strings.ToUpper(t.text)
		switch up {
		case "NULL":
			p.advance()
			return &astLit{kind: litNull, pos: t.pos}, nil
		case "TRUE", "FALSE":
			p.advance()
			return &astLit{kind: litBool, val: up == "TRUE", pos: t.pos}, nil
		case "CASE":
			return p.parseCase()
		case "CAST", "SAFE_CAST":
			if p.isOp(p.peekN(1), "(") {
				return p.parseCast(up == "SAFE_CAST")
			}
		case "EXTRACT":
			return p.parseExtract()
		case "INTERVAL":
			p.advance()
			x, err := p.parseUnary()
			if err != nil {
				return nil, err
			}
			u := p.peek()
			if u.kind != tIdent {
				return nil, p.errorf(u, "expected date part after INTERVAL expression")
			}
			p.advance()
			if p.isKw(p.peek(), "TO") {
				return nil, unimplementedf("INTERVAL ranges are not supported by the emulator")
			}
			return &astInterval{x: x, unit: strings.ToUpper(u.text), pos: t.pos}, nil
		case "ARRAY":
			p.advance()
			n := p.peek()
			switch {
			case p.isOp(n, "<"):
				p.advance()
				et, err := p.parseType()
				if err != nil {
					return nil, err
				}
				if err := p.expectGT(); err != nil {
					return nil, err
				}
				if !p.isOp(p.peek(), "[") {
					return nil, p.errorf(p.peek(), "expected \"[\" after ARRAY type")
				}
				return p.parseArrayLiteral(et, t.pos)
			case p.isOp(n, "["):
				return p.parseArrayLiteral(nil, t.pos)
			case p.isOp(n, "("):
				return nil, invalidf("ARRAY subqueries are not supported by GoogleSQL for Bigtable [at %s]", position(p.src, n.pos))
			}
			return nil, p.errorf(n, "unexpected %s after ARRAY", p.describe(n))
		case "STRUCT":
			return p.parseStruct()
		case "EXISTS":
			return nil, invalidf("EXISTS subqueries are not supported by GoogleSQL for Bigtable [at %s]", position(p.src, t.pos))
		case "SELECT":
			return nil, invalidf("subqueries are not supported by GoogleSQL for Bigtable [at %s]", position(p.src, t.pos))
		case "DATE", "TIMESTAMP":
			if n := p.peekN(1); n.kind == tString {
				p.advance()
				p.advance()
				return &astTypedLit{typ: up, val: n.str, pos: t.pos}, nil
			}
		case "NUMERIC", "BIGNUMERIC", "JSON", "DATETIME", "TIME", "RANGE":
			if n := p.peekN(1); n.kind == tString {
				return nil, unimplementedf("%s literals are not supported by the emulator", up)
			}
		}
		return p.parseIdentOrCall()
	}
	return nil, p.errorf(t, "unexpected %s", p.describe(t))
}

func (p *parser) parseIdentOrCall() (astExpr, error) {
	first := p.advance()
	parts := []token{first}
	for p.isOp(p.peek(), ".") {
		n := p.peekN(1)
		if n.kind != tIdent && n.kind != tQIdent {
			break
		}
		// Only fold into a dotted function name when followed by '('.
		p.advance()
		p.advance()
		parts = append(parts, n)
	}
	if p.isOp(p.peek(), "(") && first.kind == tIdent {
		names := make([]string, len(parts))
		for i, pt := range parts {
			names[i] = strings.ToUpper(pt.str)
		}
		return p.parseCall(strings.Join(names, "."), first.pos)
	}
	if first.kind == tIdent && isReserved(first.text) {
		return nil, p.errorf(first, "unexpected keyword %s", strings.ToUpper(first.text))
	}
	var x astExpr = &astIdent{name: first.str, quoted: first.kind == tQIdent, pos: first.pos}
	for _, pt := range parts[1:] {
		x = &astField{x: x, name: pt.str, pos: pt.pos}
	}
	return x, nil
}

func (p *parser) parseCall(name string, pos int) (astExpr, error) {
	call := &astCall{name: name, pos: pos}
	if strings.HasPrefix(name, "SAFE.") {
		call.safe = true
		call.name = strings.TrimPrefix(name, "SAFE.")
	}
	if what, ok := unsupportedFunction(call.name); ok {
		if err := p.skipBalanced(); err != nil {
			return nil, err
		}
		if _, err := p.skipOver(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(what, "window function") {
			return &astUnsupported{what: what, unimpl: true, pos: pos,
				message: "window functions are not supported by the emulator (" + call.name + ")"}, nil
		}
		return &astUnsupported{what: what, unimpl: true, pos: pos}, nil
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	if !p.acceptOp(")") {
		if p.isOp(p.peek(), "*") && p.isOp(p.peekN(1), ")") {
			p.advance()
			call.star = true
		} else {
			if p.acceptKw("DISTINCT") {
				call.distinct = true
			}
			for {
				if t := p.peek(); (t.kind == tIdent || t.kind == tQIdent) && p.isOp(p.peekN(1), "=>") {
					return nil, unimplementedf("named function arguments are not supported by the emulator (%s)", call.name)
				}
				x, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				call.args = append(call.args, x)
				if !p.acceptOp(",") {
					break
				}
			}
			if p.acceptKw("IGNORE") {
				if err := p.expectKw("NULLS"); err != nil {
					return nil, err
				}
				call.ignoreNulls = true
			} else if p.acceptKw("RESPECT") {
				if err := p.expectKw("NULLS"); err != nil {
					return nil, err
				}
				call.respectNull = true
			}
			if t := p.peek(); p.isKw(t, "HAVING") {
				return nil, unimplementedf("HAVING MAX/MIN in aggregate functions is not supported by the emulator")
			}
			if p.acceptKw("ORDER") {
				if err := p.expectKw("BY"); err != nil {
					return nil, err
				}
				items, err := p.parseOrderItems()
				if err != nil {
					return nil, err
				}
				call.orderBy = items
			}
			if p.acceptKw("LIMIT") {
				x, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				call.limit = x
			}
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	}
	over, err := p.skipOver()
	if err != nil {
		return nil, err
	}
	if over {
		return &astUnsupported{what: "window function " + call.name, unimpl: true, pos: pos,
			message: "window functions are not supported by the emulator (" + call.name + " ... OVER)"}, nil
	}
	return call, nil
}

func (p *parser) parseArrayLiteral(elemType *astType, pos int) (astExpr, error) {
	if err := p.expectOp("["); err != nil {
		return nil, err
	}
	arr := &astArray{elemType: elemType, pos: pos}
	if p.acceptOp("]") {
		return arr, nil
	}
	for {
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		arr.elems = append(arr.elems, x)
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectOp("]"); err != nil {
		return nil, err
	}
	return arr, nil
}

func (p *parser) parseStruct() (astExpr, error) {
	start := p.advance() // STRUCT
	st := &astStruct{pos: start.pos}
	if p.acceptOp("<") {
		typ := &astType{name: "STRUCT", pos: start.pos}
		if !p.isOp(p.peek(), ">") {
			fields, err := p.parseStructTypeFields()
			if err != nil {
				return nil, err
			}
			typ.fields = fields
		}
		if err := p.expectGT(); err != nil {
			return nil, err
		}
		st.typ = typ
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	if p.acceptOp(")") {
		return st, nil
	}
	for {
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		name := ""
		if p.acceptKw("AS") {
			if st.typ != nil {
				return nil, invalidf("STRUCT constructors with an explicit type cannot use AS aliases [at %s]", position(p.src, p.peek().pos))
			}
			n, err := p.identifier("field name")
			if err != nil {
				return nil, err
			}
			name = n
		} else {
			name = implicitAlias(x)
		}
		st.fields = append(st.fields, x)
		st.names = append(st.names, name)
		if !p.acceptOp(",") {
			break
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return st, nil
}

// implicitAlias returns the implicit alias for an expression per GoogleSQL rules.
func implicitAlias(x astExpr) string {
	switch e := x.(type) {
	case *astIdent:
		return e.name
	case *astField:
		return e.name
	}
	return ""
}

func (p *parser) parseStructTypeFields() ([]astTypeField, error) {
	var fields []astTypeField
	for {
		f := astTypeField{}
		t := p.peek()
		n := p.peekN(1)
		if (t.kind == tIdent || t.kind == tQIdent) && (n.kind == tIdent || n.kind == tQIdent) {
			p.advance()
			f.name = t.str
		}
		typ, err := p.parseType()
		if err != nil {
			return nil, err
		}
		f.typ = typ
		fields = append(fields, f)
		if !p.acceptOp(",") {
			return fields, nil
		}
	}
}

func (p *parser) parseType() (*astType, error) {
	t := p.peek()
	if t.kind != tIdent {
		return nil, p.errorf(t, "expected type name but got %s", p.describe(t))
	}
	p.advance()
	typ := &astType{name: strings.ToUpper(t.text), pos: t.pos}
	switch typ.name {
	case "ARRAY":
		if err := p.expectOp("<"); err != nil {
			return nil, err
		}
		e, err := p.parseType()
		if err != nil {
			return nil, err
		}
		typ.elem = e
		return typ, p.expectGT()
	case "MAP":
		if err := p.expectOp("<"); err != nil {
			return nil, err
		}
		k, err := p.parseType()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(","); err != nil {
			return nil, err
		}
		v, err := p.parseType()
		if err != nil {
			return nil, err
		}
		typ.key, typ.val = k, v
		return typ, p.expectGT()
	case "STRUCT":
		if err := p.expectOp("<"); err != nil {
			return nil, err
		}
		if !p.isOp(p.peek(), ">") {
			fs, err := p.parseStructTypeFields()
			if err != nil {
				return nil, err
			}
			typ.fields = fs
		}
		return typ, p.expectGT()
	}
	if p.isOp(p.peek(), "(") {
		return nil, unimplementedf("parameterized types such as %s(...) are not supported by the emulator", typ.name)
	}
	return typ, nil
}

func (p *parser) parseCase() (astExpr, error) {
	start := p.advance() // CASE
	c := &astCase{pos: start.pos}
	if !p.isKw(p.peek(), "WHEN") {
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.operand = x
	}
	for p.acceptKw("WHEN") {
		cond, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectKw("THEN"); err != nil {
			return nil, err
		}
		res, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.whens = append(c.whens, astWhen{cond: cond, result: res})
	}
	if len(c.whens) == 0 {
		return nil, p.errorf(p.peek(), "CASE requires at least one WHEN clause")
	}
	if p.acceptKw("ELSE") {
		x, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		c.els = x
	}
	if err := p.expectKw("END"); err != nil {
		return nil, err
	}
	return c, nil
}

func (p *parser) parseCast(safe bool) (astExpr, error) {
	start := p.advance()
	p.advance() // (
	x, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("AS"); err != nil {
		return nil, err
	}
	typ, err := p.parseType()
	if err != nil {
		return nil, err
	}
	if p.isKw(p.peek(), "FORMAT") || strings.EqualFold(p.peek().text, "FORMAT") {
		return nil, unimplementedf("CAST ... FORMAT is not supported by the emulator")
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return &astCast{x: x, typ: typ, safe: safe, pos: start.pos}, nil
}

func (p *parser) parseExtract() (astExpr, error) {
	start := p.advance() // EXTRACT
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	part, err := p.parsePostfix()
	if err != nil {
		return nil, err
	}
	if err := p.expectKw("FROM"); err != nil {
		return nil, err
	}
	x, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	ex := &astExtract{part: part, x: x, pos: start.pos}
	if p.acceptKw("AT") {
		if !strings.EqualFold(p.peek().text, "TIME") {
			return nil, p.errorf(p.peek(), "expected AT TIME ZONE")
		}
		p.advance()
		if !strings.EqualFold(p.peek().text, "ZONE") {
			return nil, p.errorf(p.peek(), "expected AT TIME ZONE")
		}
		p.advance()
		tz, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		ex.tz = tz
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return ex, nil
}
