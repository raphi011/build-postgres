// Package parser turns SQL text into syntax trees. See chapters/07-parser.
package parser

import (
	"errors"
	"fmt"
	"strings"

	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/lexer"
	"github.com/raphi011/build-postgres/internal/tuple"
)

var (
	ErrSyntax      = errors.New("syntax error")
	ErrUnknownType = errors.New("type does not exist")
	ErrStackDepth  = errors.New("stack depth limit exceeded")
)

// MaxExprDepth is how deeply expressions may nest. Every parenthesis,
// unary operator, and right operand of an infix operator costs a frame
// here and another in the analyzer and in ast.String, so a long enough
// run of "(" overflows the goroutine stack; Go reports that as a fatal
// error that no recover can catch and that takes the process with it.
// PostgreSQL: max_stack_depth, enforced by check_stack_depth.
const MaxExprDepth = 1000

// Error is a parse error at a position. It unwraps to ErrSyntax,
// ErrUnknownType, or ErrStackDepth. Expected names what the parser needed
// at Pos and Found describes the token it got instead; both are empty for
// ErrStackDepth, which is about the input as a whole. Lexical errors are
// returned as *lexer.Error, not wrapped.
type Error struct {
	Pos      lexer.Pos
	Err      error
	Expected string
	Found    string
}

func (e *Error) Error() string {
	if e.Expected == "" {
		return fmt.Sprintf("%v at line %d, column %d", e.Err, e.Pos.Line, e.Pos.Column)
	}
	return fmt.Sprintf("%v at line %d, column %d: expected %s, found %s",
		e.Err, e.Pos.Line, e.Pos.Column, e.Expected, e.Found)
}

func (e *Error) Unwrap() error { return e.Err }

// Parse parses a sequence of statements separated by semicolons. Empty
// statements are skipped, so "" and ";" parse to an empty list.
// Returns a *Error wrapping ErrSyntax with Expected "statement" if a
// statement does not start with a statement keyword, "end of statement"
// at the first token left over after one, or the token a statement rule
// needed; ErrUnknownType at an unrecognised type name; ErrStackDepth at
// the token that would nest an expression past MaxExprDepth; a
// *lexer.Error unchanged for lexical errors. The statements are nil on
// any error.
// PostgreSQL: raw_parser in parser.c.
func Parse(src string) ([]ast.Stmt, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	var stmts []ast.Stmt
	for {
		for p.tok.Kind == lexer.Semicolon {
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
		if p.tok.Kind == lexer.EOF {
			return stmts, nil
		}
		s, err := p.parseStmt()
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, s)
		if p.tok.Kind != lexer.Semicolon && p.tok.Kind != lexer.EOF {
			return nil, p.syntaxError("end of statement")
		}
	}
}

// ParseExpr parses src as a single expression that must span the whole
// input.
// Returns a *Error wrapping ErrSyntax with Expected "expression" if src
// is empty or no expression starts at the current token, "end of input"
// if tokens remain after the expression; ErrStackDepth past MaxExprDepth;
// a *lexer.Error unchanged for lexical errors.
// PostgreSQL: raw_parser with RAW_PARSE_PLPGSQL_EXPR in parser.c.
func ParseExpr(src string) (ast.Expr, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	e, err := p.parseExpr(0)
	if err != nil {
		return nil, err
	}
	if p.tok.Kind != lexer.EOF {
		return nil, p.syntaxError("end of input")
	}
	return e, nil
}

// parser holds the lexer and one token of lookahead.
type parser struct {
	lex   *lexer.Lexer
	tok   lexer.Token
	depth int // expressions currently open, capped at MaxExprDepth
}

func newParser(src string) (*parser, error) {
	p := &parser{lex: lexer.New(src)}
	if err := p.advance(); err != nil {
		return nil, err
	}
	return p, nil
}

// advance reads the next token into p.tok.
func (p *parser) advance() error {
	tok, err := p.lex.Next()
	if err != nil {
		return err
	}
	p.tok = tok
	return nil
}

// next returns the current token and advances past it.
func (p *parser) next() (lexer.Token, error) {
	tok := p.tok
	return tok, p.advance()
}

func (p *parser) isKeyword(word string) bool {
	return p.tok.Kind == lexer.Keyword && p.tok.Text == word
}

// accept consumes the current token if it is the given keyword.
func (p *parser) accept(word string) (bool, error) {
	if !p.isKeyword(word) {
		return false, nil
	}
	return true, p.advance()
}

// expect consumes the given keyword or fails.
func (p *parser) expect(word string) error {
	if !p.isKeyword(word) {
		return p.syntaxError(strings.ToUpper(word))
	}
	return p.advance()
}

// expectKind consumes a token of the given kind or fails.
func (p *parser) expectKind(k lexer.Kind) (lexer.Token, error) {
	if p.tok.Kind != k {
		return lexer.Token{}, p.syntaxError(k.String())
	}
	return p.next()
}

func (p *parser) ident() (string, error) {
	tok, err := p.expectKind(lexer.Ident)
	return tok.Text, err
}

func (p *parser) syntaxError(expected string) error {
	return &Error{Pos: p.tok.Pos, Err: ErrSyntax, Expected: expected, Found: describe(p.tok)}
}

// describe renders a token for an error message.
func describe(tok lexer.Token) string {
	switch tok.Kind {
	case lexer.EOF:
		return "end of input"
	case lexer.String:
		return ast.QuoteString(tok.Text)
	}
	return `"` + tok.Text + `"`
}

func (p *parser) parseStmt() (ast.Stmt, error) {
	if p.tok.Kind != lexer.Keyword {
		return nil, p.syntaxError("statement")
	}
	switch p.tok.Text {
	case "create":
		return p.parseCreateTable()
	case "drop":
		return p.parseDropTable()
	case "insert":
		return p.parseInsert()
	case "select":
		return p.parseSelect()
	case "update":
		return p.parseUpdate()
	case "delete":
		return p.parseDelete()
	case "begin":
		return &ast.Begin{}, p.advance()
	case "commit":
		return &ast.Commit{}, p.advance()
	case "rollback":
		return &ast.Rollback{}, p.advance()
	case "explain":
		return p.parseExplain()
	}
	return nil, p.syntaxError("statement")
}

func (p *parser) parseCreateTable() (ast.Stmt, error) {
	if err := p.expect("create"); err != nil {
		return nil, err
	}
	if err := p.expect("table"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	if _, err := p.expectKind(lexer.LParen); err != nil {
		return nil, err
	}
	s := &ast.CreateTable{Name: name}
	for {
		col, err := p.parseColumnDef()
		if err != nil {
			return nil, err
		}
		s.Columns = append(s.Columns, col)
		if p.tok.Kind != lexer.Comma {
			break
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if _, err := p.expectKind(lexer.RParen); err != nil {
		return nil, err
	}
	return s, nil
}

// typeNames maps the accepted type names to their types.
var typeNames = map[string]tuple.TypeID{
	"int4": tuple.Int4, "int": tuple.Int4, "integer": tuple.Int4,
	"int8": tuple.Int8, "bigint": tuple.Int8,
	"bool": tuple.Bool, "boolean": tuple.Bool,
	"text": tuple.Text,
}

func (p *parser) parseColumnDef() (ast.ColumnDef, error) {
	var col ast.ColumnDef
	name, err := p.ident()
	if err != nil {
		return col, err
	}
	col.Name = name
	if p.tok.Kind != lexer.Ident {
		return col, p.syntaxError("type name")
	}
	typ, ok := typeNames[p.tok.Text]
	if !ok {
		return col, &Error{Pos: p.tok.Pos, Err: ErrUnknownType, Expected: "type name", Found: describe(p.tok)}
	}
	col.Type = typ
	if err := p.advance(); err != nil {
		return col, err
	}
	for {
		switch {
		case p.isKeyword("not"):
			if err := p.advance(); err != nil {
				return col, err
			}
			if err := p.expect("null"); err != nil {
				return col, err
			}
			col.NotNull = true
		case p.isKeyword("primary"):
			if err := p.advance(); err != nil {
				return col, err
			}
			if err := p.expect("key"); err != nil {
				return col, err
			}
			col.PrimaryKey = true
		default:
			return col, nil
		}
	}
}

func (p *parser) parseDropTable() (ast.Stmt, error) {
	if err := p.expect("drop"); err != nil {
		return nil, err
	}
	if err := p.expect("table"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	return &ast.DropTable{Name: name}, nil
}

func (p *parser) parseInsert() (ast.Stmt, error) {
	if err := p.expect("insert"); err != nil {
		return nil, err
	}
	if err := p.expect("into"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	s := &ast.Insert{Table: name}
	if p.tok.Kind == lexer.LParen {
		if err := p.advance(); err != nil {
			return nil, err
		}
		s.Columns = []string{}
		for {
			col, err := p.ident()
			if err != nil {
				return nil, err
			}
			s.Columns = append(s.Columns, col)
			if p.tok.Kind != lexer.Comma {
				break
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
		if _, err := p.expectKind(lexer.RParen); err != nil {
			return nil, err
		}
	}
	if err := p.expect("values"); err != nil {
		return nil, err
	}
	for {
		if _, err := p.expectKind(lexer.LParen); err != nil {
			return nil, err
		}
		row, err := p.parseExprList()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectKind(lexer.RParen); err != nil {
			return nil, err
		}
		s.Rows = append(s.Rows, row)
		if p.tok.Kind != lexer.Comma {
			return s, nil
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
}

// parseExprList parses one or more comma-separated expressions.
func (p *parser) parseExprList() ([]ast.Expr, error) {
	var list []ast.Expr
	for {
		e, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		list = append(list, e)
		if p.tok.Kind != lexer.Comma {
			return list, nil
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
}

func (p *parser) parseSelect() (ast.Stmt, error) {
	if err := p.expect("select"); err != nil {
		return nil, err
	}
	s := &ast.Select{}
	for {
		item, err := p.parseSelectItem()
		if err != nil {
			return nil, err
		}
		s.Items = append(s.Items, item)
		if p.tok.Kind != lexer.Comma {
			break
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if ok, err := p.accept("from"); err != nil {
		return nil, err
	} else if ok {
		for {
			t, err := p.parseTableExpr()
			if err != nil {
				return nil, err
			}
			s.From = append(s.From, t)
			if p.tok.Kind != lexer.Comma {
				break
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
	}
	where, err := p.parseWhere()
	if err != nil {
		return nil, err
	}
	s.Where = where
	if ok, err := p.accept("order"); err != nil {
		return nil, err
	} else if ok {
		if err := p.expect("by"); err != nil {
			return nil, err
		}
		for {
			e, err := p.parseExpr(0)
			if err != nil {
				return nil, err
			}
			item := ast.OrderItem{Expr: e}
			if ok, err := p.accept("asc"); err != nil {
				return nil, err
			} else if !ok {
				if item.Desc, err = p.accept("desc"); err != nil {
					return nil, err
				}
			}
			s.OrderBy = append(s.OrderBy, item)
			if p.tok.Kind != lexer.Comma {
				break
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
	}
	if ok, err := p.accept("limit"); err != nil {
		return nil, err
	} else if ok {
		if s.Limit, err = p.parseExpr(0); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (p *parser) parseSelectItem() (ast.SelectItem, error) {
	if p.tok.Kind == lexer.Star {
		tok, err := p.next()
		return ast.SelectItem{Expr: &ast.Star{Loc: tok.Pos}}, err
	}
	e, err := p.parseExpr(0)
	if err != nil {
		return ast.SelectItem{}, err
	}
	alias, err := p.parseAlias()
	return ast.SelectItem{Expr: e, Alias: alias}, err
}

// parseAlias parses an optional [AS] identifier.
func (p *parser) parseAlias() (string, error) {
	if ok, err := p.accept("as"); err != nil {
		return "", err
	} else if ok {
		return p.ident()
	}
	if p.tok.Kind == lexer.Ident {
		return p.ident()
	}
	return "", nil
}

// parseTableExpr parses a table reference followed by any number of
// JOIN ... ON clauses.
func (p *parser) parseTableExpr() (ast.TableExpr, error) {
	ref, err := p.parseTableRef()
	if err != nil {
		return nil, err
	}
	var left ast.TableExpr = ref
	for {
		ok, err := p.accept("join")
		if err != nil {
			return nil, err
		}
		if !ok {
			return left, nil
		}
		right, err := p.parseTableRef()
		if err != nil {
			return nil, err
		}
		if err := p.expect("on"); err != nil {
			return nil, err
		}
		on, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		left = &ast.Join{Left: left, Right: right, On: on}
	}
}

func (p *parser) parseTableRef() (*ast.TableRef, error) {
	tok, err := p.expectKind(lexer.Ident)
	if err != nil {
		return nil, err
	}
	alias, err := p.parseAlias()
	if err != nil {
		return nil, err
	}
	return &ast.TableRef{Loc: tok.Pos, Name: tok.Text, Alias: alias}, nil
}

// parseWhere parses an optional WHERE clause.
func (p *parser) parseWhere() (ast.Expr, error) {
	if ok, err := p.accept("where"); err != nil || !ok {
		return nil, err
	}
	return p.parseExpr(0)
}

func (p *parser) parseUpdate() (ast.Stmt, error) {
	if err := p.expect("update"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	if err := p.expect("set"); err != nil {
		return nil, err
	}
	s := &ast.Update{Table: name}
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		if _, err := p.expectKind(lexer.Eq); err != nil {
			return nil, err
		}
		val, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		s.Set = append(s.Set, ast.Assignment{Column: col, Value: val})
		if p.tok.Kind != lexer.Comma {
			break
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	s.Where, err = p.parseWhere()
	return s, err
}

func (p *parser) parseDelete() (ast.Stmt, error) {
	if err := p.expect("delete"); err != nil {
		return nil, err
	}
	if err := p.expect("from"); err != nil {
		return nil, err
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	s := &ast.Delete{Table: name}
	s.Where, err = p.parseWhere()
	return s, err
}

func (p *parser) parseExplain() (ast.Stmt, error) {
	if err := p.expect("explain"); err != nil {
		return nil, err
	}
	if !p.isKeyword("select") && !p.isKeyword("insert") && !p.isKeyword("update") && !p.isKeyword("delete") {
		return nil, p.syntaxError("SELECT, INSERT, UPDATE, or DELETE")
	}
	s, err := p.parseStmt()
	if err != nil {
		return nil, err
	}
	return &ast.Explain{Stmt: s}, nil
}

// Binding powers, lowest first. PostgreSQL's table from gram.y:
// OR < AND < NOT < IS < comparison < + - < * / < unary minus.
const (
	precOr = iota + 1
	precAnd
	precNot
	precIs
	precCmp
	precAdd
	precMul
	precNeg
)

// binOps maps operator tokens and keywords to the operator and its
// precedence.
type binOp struct {
	op   ast.BinOp
	prec int
}

var binOpTokens = map[lexer.Kind]binOp{
	lexer.Eq: {ast.Eq, precCmp}, lexer.Ne: {ast.Ne, precCmp},
	lexer.Lt: {ast.Lt, precCmp}, lexer.Le: {ast.Le, precCmp},
	lexer.Gt: {ast.Gt, precCmp}, lexer.Ge: {ast.Ge, precCmp},
	lexer.Plus: {ast.Add, precAdd}, lexer.Minus: {ast.Sub, precAdd},
	lexer.Star: {ast.Mul, precMul}, lexer.Slash: {ast.Div, precMul},
}

var binOpKeywords = map[string]binOp{
	"and": {ast.And, precAnd},
	"or":  {ast.Or, precOr},
}

// infixOp returns the binary operator the current token denotes, if any.
func (p *parser) infixOp() (binOp, bool) {
	if p.tok.Kind == lexer.Keyword {
		op, ok := binOpKeywords[p.tok.Text]
		return op, ok
	}
	op, ok := binOpTokens[p.tok.Kind]
	return op, ok
}

// parseExpr is a Pratt parser: it parses a prefix expression, then keeps
// absorbing infix and postfix operators whose precedence is at least
// minPrec. Comparison operators do not chain, as in PostgreSQL.
func (p *parser) parseExpr(minPrec int) (ast.Expr, error) {
	// Checked on the way in, before the recursion it guards.
	if p.depth++; p.depth > MaxExprDepth {
		return nil, &Error{Pos: p.tok.Pos, Err: ErrStackDepth}
	}
	defer func() { p.depth-- }()

	left, err := p.parsePrefix()
	if err != nil {
		return nil, err
	}
	for {
		if p.isKeyword("is") {
			if precIs < minPrec {
				return left, nil
			}
			left, err = p.parseIsNull(left)
			if err != nil {
				return nil, err
			}
			continue
		}
		op, ok := p.infixOp()
		if !ok || op.prec < minPrec {
			return left, nil
		}
		tok, err := p.next()
		if err != nil {
			return nil, err
		}
		right, err := p.parseExpr(op.prec + 1)
		if err != nil {
			return nil, err
		}
		left = &ast.BinaryExpr{Loc: tok.Pos, Op: op.op, Left: left, Right: right}
		if op.prec == precCmp {
			if next, ok := p.infixOp(); ok && next.prec == precCmp {
				return nil, p.syntaxError("end of expression")
			}
		}
	}
}

// parseIsNull parses IS [NOT] NULL after its operand.
func (p *parser) parseIsNull(x ast.Expr) (ast.Expr, error) {
	tok, err := p.next()
	if err != nil {
		return nil, err
	}
	not, err := p.accept("not")
	if err != nil {
		return nil, err
	}
	if err := p.expect("null"); err != nil {
		return nil, err
	}
	return &ast.IsNull{Loc: tok.Pos, X: x, Not: not}, nil
}

// parsePrefix parses a literal, column reference, parenthesised
// expression, or prefix operator application.
func (p *parser) parsePrefix() (ast.Expr, error) {
	tok := p.tok
	switch tok.Kind {
	case lexer.Integer:
		return &ast.IntLit{Loc: tok.Pos, Text: tok.Text}, p.advance()
	case lexer.String:
		return &ast.StrLit{Loc: tok.Pos, Value: tok.Text}, p.advance()
	case lexer.Ident:
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.tok.Kind != lexer.Dot {
			return &ast.ColumnRef{Loc: tok.Pos, Name: tok.Text}, nil
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		name, err := p.ident()
		if err != nil {
			return nil, err
		}
		return &ast.ColumnRef{Loc: tok.Pos, Table: tok.Text, Name: name}, nil
	case lexer.LParen:
		if err := p.advance(); err != nil {
			return nil, err
		}
		e, err := p.parseExpr(0)
		if err != nil {
			return nil, err
		}
		if _, err := p.expectKind(lexer.RParen); err != nil {
			return nil, err
		}
		return e, nil
	case lexer.Minus:
		if err := p.advance(); err != nil {
			return nil, err
		}
		x, err := p.parseExpr(precNeg)
		if err != nil {
			return nil, err
		}
		return &ast.UnaryExpr{Loc: tok.Pos, Op: ast.Neg, X: x}, nil
	case lexer.Keyword:
		switch tok.Text {
		case "true":
			return &ast.BoolLit{Loc: tok.Pos, Value: true}, p.advance()
		case "false":
			return &ast.BoolLit{Loc: tok.Pos, Value: false}, p.advance()
		case "null":
			return &ast.NullLit{Loc: tok.Pos}, p.advance()
		case "not":
			if err := p.advance(); err != nil {
				return nil, err
			}
			x, err := p.parseExpr(precNot)
			if err != nil {
				return nil, err
			}
			return &ast.UnaryExpr{Loc: tok.Pos, Op: ast.Not, X: x}, nil
		}
	}
	return nil, p.syntaxError("expression")
}
