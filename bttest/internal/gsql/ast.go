package gsql

// AST produced by the parser.

type astExpr interface{ position() int }

type litKind uint8

const (
	litNone litKind = iota
	litNull
	litBool
	litInt
	litFloat
	litString
	litBytes
)

type (
	astLit struct {
		kind litKind
		val  any // nil, bool, int64, float64, string, []byte
		pos  int
	}
	// astTypedLit is DATE '...' / TIMESTAMP '...'.
	astTypedLit struct {
		typ string
		val string
		pos int
	}
	astParam struct {
		name string
		pos  int
	}
	astIdent struct {
		name   string
		quoted bool
		pos    int
	}
	astField struct {
		x    astExpr
		name string
		pos  int
	}
	astIndex struct {
		x    astExpr
		mode string // "", OFFSET, SAFE_OFFSET, ORDINAL, SAFE_ORDINAL, KEY, SAFE_KEY
		idx  astExpr
		pos  int
	}
	astUnary struct {
		op  string // "-", "+", "~", "NOT"
		x   astExpr
		pos int
	}
	astBinary struct {
		op   string // + - * / || = != < <= > >= AND OR & | ^ << >>
		l, r astExpr
		pos  int
	}
	astIs struct {
		x    astExpr
		not  bool
		what string // NULL, TRUE, FALSE
		pos  int
	}
	astIn struct {
		x      astExpr
		not    bool
		list   []astExpr
		unnest astExpr
		pos    int
	}
	astBetween struct {
		x, lo, hi astExpr
		not       bool
		pos       int
	}
	astLike struct {
		x, pattern astExpr
		not        bool
		pos        int
	}
	astWhen struct {
		cond, result astExpr
	}
	astCase struct {
		operand astExpr
		whens   []astWhen
		els     astExpr
		pos     int
	}
	astOrderItem struct {
		x         astExpr
		desc      bool
		explicit  bool // ASC/DESC written explicitly
		nulls     string
		nullsSeen bool
	}
	astCall struct {
		name        string // upper-case, possibly dotted (HLL_COUNT.INIT)
		args        []astExpr
		star        bool // COUNT(*)
		distinct    bool
		ignoreNulls bool
		respectNull bool
		orderBy     []astOrderItem
		limit       astExpr
		safe        bool // SAFE. prefix
		pos         int
	}
	astCast struct {
		x    astExpr
		typ  *astType
		safe bool
		pos  int
	}
	astExtract struct {
		part astExpr // date part (identifier or WEEK(x) call)
		x    astExpr
		tz   astExpr
		pos  int
	}
	astInterval struct {
		x    astExpr
		unit string
		pos  int
	}
	astArray struct {
		elemType *astType
		elems    []astExpr
		pos      int
	}
	astStruct struct {
		typ    *astType // typed struct syntax
		fields []astExpr
		names  []string
		tuple  bool
		pos    int
	}
	// astUnsupported records a construct that parsed but cannot be executed.
	astUnsupported struct {
		what    string
		unimpl  bool // Unimplemented (true) vs InvalidArgument (false)
		pos     int
		message string
	}
)

func (e *astLit) position() int         { return e.pos }
func (e *astTypedLit) position() int    { return e.pos }
func (e *astParam) position() int       { return e.pos }
func (e *astIdent) position() int       { return e.pos }
func (e *astField) position() int       { return e.pos }
func (e *astIndex) position() int       { return e.pos }
func (e *astUnary) position() int       { return e.pos }
func (e *astBinary) position() int      { return e.pos }
func (e *astIs) position() int          { return e.pos }
func (e *astIn) position() int          { return e.pos }
func (e *astBetween) position() int     { return e.pos }
func (e *astLike) position() int        { return e.pos }
func (e *astCase) position() int        { return e.pos }
func (e *astCall) position() int        { return e.pos }
func (e *astCast) position() int        { return e.pos }
func (e *astExtract) position() int     { return e.pos }
func (e *astInterval) position() int    { return e.pos }
func (e *astArray) position() int       { return e.pos }
func (e *astStruct) position() int      { return e.pos }
func (e *astUnsupported) position() int { return e.pos }

type astType struct {
	name   string // INT64, ARRAY, STRUCT, MAP, ...
	elem   *astType
	key    *astType
	val    *astType
	fields []astTypeField
	pos    int
}

type astTypeField struct {
	name string
	typ  *astType
}

type astReplace struct {
	x    astExpr
	name string
}

type astSelectItem struct {
	star       bool    // * or expr.*
	starExpr   astExpr // expr in expr.* (nil for bare *)
	except     []string
	replace    []astReplace
	expr       astExpr
	alias      string
	hasAlias   bool
	pos        int
	exprSource string // original text, used for messages
}

type astNamedArg struct {
	name string
	x    astExpr
	pos  int
}

type astFrom struct {
	table     string
	tableArgs []astNamedArg
	hasArgs   bool
	alias     string
	unpack    *astQuery
	hints     []astNamedArg
	pos       int
}

type astSelect struct {
	distinct bool
	items    []astSelectItem
	from     *astFrom
	where    astExpr
	groupBy  []astExpr
	having   astExpr
	pos      int
}

type astQuery struct {
	sel     *astSelect
	inner   *astQuery
	orderBy []astOrderItem
	limit   astExpr
	offset  astExpr
	pos     int
}
