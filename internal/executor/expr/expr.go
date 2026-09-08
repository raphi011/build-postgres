// Package expr evaluates bound expressions against a row by compiling
// them into closures. See chapters/10-expressions.
package expr

import (
	"errors"

	"github.com/raphi011/build-postgres/internal/sql/query"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// Sentinel errors, one per PostgreSQL error class raised at evaluation.
var (
	ErrOutOfRange     = errors.New("out of range")     // 22003
	ErrDivisionByZero = errors.New("division by zero") // 22012
)

// Error is an evaluation error. Msg is PostgreSQL's message text. There
// is no position: PostgreSQL reports runtime errors without one.
type Error struct {
	Err error
	Msg string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// Row is what an expression is evaluated against: one value and one null
// flag per slot. Slot i of a row built from a range table is column
// Layout[rel]+attr; see Layout.
type Row struct {
	Values []tuple.Datum
	Nulls  []bool
}

// Layout maps the columns of a range table onto row slots. Element i is
// the slot of the first column of range entry i, and the final element
// is the total number of slots, so a Layout has one more element than
// the range table it was built from.
type Layout []int

// NewLayout lays out the columns of every range entry in order.
// NewLayout(nil) is Layout{0}: no columns, width zero.
func NewLayout(rng []*query.RangeEntry) Layout {
	panic("not implemented")
}

// Slot returns the row slot of v.
func (l Layout) Slot(v *query.Var) int {
	panic("not implemented")
}

// Width returns the number of slots in a row of this layout.
func (l Layout) Width() int {
	panic("not implemented")
}

// Func is a compiled expression. It returns the value and whether it is
// NULL; the value is undefined when null is set. Errors are *Error.
type Func func(r Row) (v tuple.Datum, null bool, err error)

// Qual evaluates f as a WHERE condition: true only if the result is
// TRUE, so NULL counts as false. An evaluation error is returned as is,
// with false.
// PostgreSQL: ExecQual in executor.h.
func (f Func) Qual(r Row) (bool, error) {
	panic("not implemented")
}

// Compile turns e into a Func. Vars are resolved through l, which may
// be nil when e has no Var. Panics on a node kind it does not know.
// The Func returns *Error: ErrOutOfRange with "integer out of range" or
// "bigint out of range" when an int4 or int8 result does not fit, on
// MinInt / -1, and on -MinInt; ErrDivisionByZero with "division by
// zero" on a zero divisor, checked before the MinInt / -1 case.
// PostgreSQL: ExecInitExpr in execExpr.c.
func Compile(e query.Expr, l Layout) Func {
	panic("not implemented")
}

// Compare orders two non-null values of the same type: integers
// numerically, booleans with false first, text bytewise. It returns -1,
// 0, or 1. Mixed or unknown types panic; the analyzer rules them out.
// PostgreSQL: btint4cmp in int.c, btint8cmp in int8.c, btboolcmp in
// bool.c, varstr_cmp in varlena.c.
func Compare(a, b tuple.Datum) int {
	panic("not implemented")
}
