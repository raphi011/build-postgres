// Package expr evaluates bound expressions against a row by compiling
// them into closures. See chapters/10-expressions.
package expr

import (
	"errors"
	"fmt"
	"math"

	"github.com/raphi011/build-postgres/internal/sql/ast"
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

func outOfRange(t tuple.TypeID) error {
	name := "integer"
	if t == tuple.Int8 {
		name = "bigint"
	}
	return &Error{Err: ErrOutOfRange, Msg: name + " out of range"}
}

func divisionByZero() error {
	return &Error{Err: ErrDivisionByZero, Msg: "division by zero"}
}

// Row is what an expression is evaluated against: one value and one null
// flag per slot. Slot i of a row built from a range table is column
// Layout[rel]+attr; see Layout.
type Row struct {
	Values []tuple.Datum
	Nulls  []bool
}

// Layout maps the columns of a range table onto row slots. Element i is
// the slot of the first column of the range entry whose Index is i, -1
// for an entry the row does not hold, and the final element is the
// total number of slots, so a Layout has two more elements than the
// highest Index it holds. A row built from the range table itself has
// its entries in Index order; a join's output (chapter 15) holds the
// outer relations' columns before the inner's, whatever their Index.
type Layout []int

// NewLayout lays out the columns of the given range entries in the
// given order, at the positions their Index says. NewLayout(nil) is
// Layout{0}: no columns, width zero.
func NewLayout(rng []*query.RangeEntry) Layout {
	l := make(Layout, len(rng)+1)
	for i, e := range rng {
		l[i+1] = l[i] + e.Rel.Desc.Len()
	}
	return l
}

// Slot returns the row slot of v.
func (l Layout) Slot(v *query.Var) int {
	return l[v.Rel] + v.Attr
}

// Width returns the number of slots in a row of this layout.
func (l Layout) Width() int {
	return l[len(l)-1]
}

// Func is a compiled expression. It returns the value and whether it is
// NULL; the value is undefined when null is set. Errors are *Error.
type Func func(r Row) (v tuple.Datum, null bool, err error)

// Qual evaluates f as a WHERE condition: true only if the result is
// TRUE, so NULL counts as false. An evaluation error is returned as is,
// with false.
// PostgreSQL: ExecQual in executor.h.
func (f Func) Qual(r Row) (bool, error) {
	v, null, err := f(r)
	if err != nil || null {
		return false, err
	}
	return v.(bool), nil
}

// Compile turns e into a Func. Vars are resolved through l, which may
// be nil when e has no Var. Panics on a node kind it does not know.
// The Func returns *Error: ErrOutOfRange with "integer out of range" or
// "bigint out of range" when an int4 or int8 result does not fit, on
// MinInt / -1, and on -MinInt; ErrDivisionByZero with "division by
// zero" on a zero divisor, checked before the MinInt / -1 case.
// PostgreSQL: ExecInitExpr in execExpr.c.
func Compile(e query.Expr, l Layout) Func {
	switch e := e.(type) {
	case *query.Var:
		slot := l.Slot(e)
		return func(r Row) (tuple.Datum, bool, error) {
			return r.Values[slot], r.Nulls[slot], nil
		}
	case *query.Const:
		v, null := e.Value, e.Null
		return func(Row) (tuple.Datum, bool, error) { return v, null, nil }
	case *query.OpExpr:
		return compileOp(e, l)
	case *query.BoolExpr:
		return compileBool(e, l)
	case *query.Neg:
		return compileNeg(e, l)
	case *query.NullTest:
		x := Compile(e.X, l)
		not := e.Not
		return func(r Row) (tuple.Datum, bool, error) {
			_, null, err := x(r)
			if err != nil {
				return nil, false, err
			}
			return null != not, false, nil
		}
	case *query.Cast:
		x := Compile(e.X, l)
		return func(r Row) (tuple.Datum, bool, error) {
			v, null, err := x(r)
			if err != nil || null {
				return nil, null, err
			}
			return int64(v.(int32)), false, nil
		}
	}
	panic(fmt.Sprintf("expr: cannot compile %T", e))
}

// compileOp compiles arithmetic and comparison. Both are strict: a NULL
// operand yields NULL.
func compileOp(e *query.OpExpr, l Layout) Func {
	left, right := Compile(e.Left, l), Compile(e.Right, l)
	var op func(a, b tuple.Datum) (tuple.Datum, error)
	switch e.Op {
	case ast.Add, ast.Sub, ast.Mul, ast.Div:
		if e.Left.Type() == tuple.Int8 {
			op = int8Op(e.Op)
		} else {
			op = int4Op(e.Op)
		}
	default:
		op = compareOp(e.Op)
	}
	return func(r Row) (tuple.Datum, bool, error) {
		a, null, err := left(r)
		if err != nil || null {
			return nil, null, err
		}
		b, null, err := right(r)
		if err != nil || null {
			return nil, null, err
		}
		v, err := op(a, b)
		return v, false, err
	}
}

// int4Op does int4 arithmetic in 64 bits and range-checks the result.
func int4Op(op ast.BinOp) func(a, b tuple.Datum) (tuple.Datum, error) {
	return func(a, b tuple.Datum) (tuple.Datum, error) {
		x, y := int64(a.(int32)), int64(b.(int32))
		var res int64
		switch op {
		case ast.Add:
			res = x + y
		case ast.Sub:
			res = x - y
		case ast.Mul:
			res = x * y
		case ast.Div:
			if y == 0 {
				return nil, divisionByZero()
			}
			res = x / y
		}
		if res < math.MinInt32 || res > math.MaxInt32 {
			return nil, outOfRange(tuple.Int4)
		}
		return int32(res), nil
	}
}

// int8Op does int8 arithmetic with the overflow tests of
// include/common/int.h.
func int8Op(op ast.BinOp) func(a, b tuple.Datum) (tuple.Datum, error) {
	return func(a, b tuple.Datum) (tuple.Datum, error) {
		x, y := a.(int64), b.(int64)
		switch op {
		case ast.Add:
			res := x + y
			if (res > x) != (y > 0) {
				return nil, outOfRange(tuple.Int8)
			}
			return res, nil
		case ast.Sub:
			res := x - y
			if (res < x) != (y > 0) {
				return nil, outOfRange(tuple.Int8)
			}
			return res, nil
		case ast.Mul:
			res := x * y
			if x != 0 && (res/x != y || (x == -1 && y == math.MinInt64)) {
				return nil, outOfRange(tuple.Int8)
			}
			return res, nil
		case ast.Div:
			if y == 0 {
				return nil, divisionByZero()
			}
			if y == -1 && x == math.MinInt64 {
				return nil, outOfRange(tuple.Int8)
			}
			return x / y, nil
		}
		panic("expr: not an arithmetic operator")
	}
}

// compareOp compares two values of the same type.
func compareOp(op ast.BinOp) func(a, b tuple.Datum) (tuple.Datum, error) {
	return func(a, b tuple.Datum) (tuple.Datum, error) {
		c := Compare(a, b)
		switch op {
		case ast.Eq:
			return c == 0, nil
		case ast.Ne:
			return c != 0, nil
		case ast.Lt:
			return c < 0, nil
		case ast.Le:
			return c <= 0, nil
		case ast.Gt:
			return c > 0, nil
		case ast.Ge:
			return c >= 0, nil
		}
		panic("expr: not a comparison operator")
	}
}

// compileBool implements three-valued AND, OR, and NOT. AND stops at the
// first FALSE and OR at the first TRUE; NULL keeps going.
func compileBool(e *query.BoolExpr, l Layout) Func {
	args := make([]Func, len(e.Args))
	for i, a := range e.Args {
		args[i] = Compile(a, l)
	}
	if e.Op == query.Not {
		x := args[0]
		return func(r Row) (tuple.Datum, bool, error) {
			v, null, err := x(r)
			if err != nil || null {
				return nil, null, err
			}
			return !v.(bool), false, nil
		}
	}
	// AND decides on FALSE, OR decides on TRUE.
	decider := e.Op == query.Or
	return func(r Row) (tuple.Datum, bool, error) {
		anyNull := false
		for _, a := range args {
			v, null, err := a(r)
			if err != nil {
				return nil, false, err
			}
			if null {
				anyNull = true
				continue
			}
			if v.(bool) == decider {
				return decider, false, nil
			}
		}
		if anyNull {
			return nil, true, nil
		}
		return !decider, false, nil
	}
}

// compileNeg negates an integer, failing on the minimum value.
func compileNeg(e *query.Neg, l Layout) Func {
	x := Compile(e.X, l)
	if e.X.Type() == tuple.Int8 {
		return func(r Row) (tuple.Datum, bool, error) {
			v, null, err := x(r)
			if err != nil || null {
				return nil, null, err
			}
			n := v.(int64)
			if n == math.MinInt64 {
				return nil, false, outOfRange(tuple.Int8)
			}
			return -n, false, nil
		}
	}
	return func(r Row) (tuple.Datum, bool, error) {
		v, null, err := x(r)
		if err != nil || null {
			return nil, null, err
		}
		n := v.(int32)
		if n == math.MinInt32 {
			return nil, false, outOfRange(tuple.Int4)
		}
		return -n, false, nil
	}
}

// Compare orders two non-null values of the same type: integers
// numerically, booleans with false first, text bytewise. It returns -1,
// 0, or 1. Mixed or unknown types panic; the analyzer rules them out.
// PostgreSQL: btint4cmp in int.c, btint8cmp in int8.c, btboolcmp in
// bool.c, varstr_cmp in varlena.c.
func Compare(a, b tuple.Datum) int {
	switch x := a.(type) {
	case int32:
		return cmp(x, b.(int32))
	case int64:
		return cmp(x, b.(int64))
	case bool:
		y := b.(bool)
		switch {
		case x == y:
			return 0
		case y:
			return -1
		}
		return 1
	case string:
		return cmp(x, b.(string))
	}
	panic(fmt.Sprintf("expr: cannot compare %T", a))
}

func cmp[T int32 | int64 | string](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
