// Package analyzer binds a syntax tree to the catalog: it resolves table
// and column names, checks and assigns types, and produces a query.Stmt.
// See chapters/09-analyzer.
package analyzer

import (
	"errors"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/lexer"
	"github.com/raphi011/build-postgres/internal/sql/query"
)

// Catalog is what the analyzer needs from the system catalog. Lookup
// returns catalog.ErrNotFound for an unknown table.
type Catalog interface {
	Lookup(name string) (*catalog.RelationInfo, error)
}

// Sentinel errors, one per PostgreSQL error class the analyzer raises.
var (
	ErrUndefinedTable    = errors.New("undefined table")          // 42P01
	ErrUndefinedColumn   = errors.New("undefined column")         // 42703
	ErrAmbiguousColumn   = errors.New("ambiguous column")         // 42702
	ErrDuplicateAlias    = errors.New("duplicate alias")          // 42712
	ErrDuplicateColumn   = errors.New("duplicate column")         // 42701
	ErrUndefinedOperator = errors.New("undefined operator")       // 42883
	ErrTypeMismatch      = errors.New("datatype mismatch")        // 42804
	ErrInvalidColumnRef  = errors.New("invalid column ref")       // 42P10
	ErrInvalidInput      = errors.New("invalid input syntax")     // 22P02
	ErrOutOfRange        = errors.New("out of range")             // 22003
	ErrNotNull           = errors.New("not-null violation")       // 23502
	ErrSyntax            = errors.New("syntax error")             // 42601
	ErrTableDefinition   = errors.New("invalid table definition") // 42P16
)

// Error is an analysis error. Msg is PostgreSQL's message text. Pos is
// where the REPL should point; Pos.Line == 0 means there is no position.
type Error struct {
	Pos lexer.Pos
	Err error
	Msg string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// Analyze binds stmt against cat. Every failure is a *Error carrying the
// sentinel, PostgreSQL's message, and the position of the offending
// token (Pos.Line == 0 where PostgreSQL reports none: NOT NULL
// violations, multiple assignments, multiple primary keys). A catalog
// error other than catalog.ErrNotFound is returned unchanged. A nil or
// unknown statement is an error, not a panic.
// PostgreSQL: transformStmt in analyze.c.
func Analyze(stmt ast.Stmt, cat Catalog) (query.Stmt, error) {
	panic("not implemented")
}
