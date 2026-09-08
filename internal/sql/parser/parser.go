// Package parser turns SQL text into syntax trees. See chapters/07-parser.
package parser

import (
	"errors"
	"fmt"

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
	panic("not implemented")
}

// ParseExpr parses src as a single expression that must span the whole
// input.
// Returns a *Error wrapping ErrSyntax with Expected "expression" if src
// is empty or no expression starts at the current token, "end of input"
// if tokens remain after the expression; ErrStackDepth past MaxExprDepth;
// a *lexer.Error unchanged for lexical errors.
// PostgreSQL: raw_parser with RAW_PARSE_PLPGSQL_EXPR in parser.c.
func ParseExpr(src string) (ast.Expr, error) {
	panic("not implemented")
}

// parser holds the lexer and one token of lookahead.
type parser struct {
	lex *lexer.Lexer
	tok lexer.Token
}

// typeNames maps the accepted type names to their types.
var typeNames = map[string]tuple.TypeID{
	"int4": tuple.Int4, "int": tuple.Int4, "integer": tuple.Int4,
	"int8": tuple.Int8, "bigint": tuple.Int8,
	"bool": tuple.Bool, "boolean": tuple.Bool,
	"text": tuple.Text,
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
