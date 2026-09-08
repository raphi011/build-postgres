// Package query defines the bound statement tree the analyzer produces:
// tables resolved to catalog entries, columns to range table positions,
// and every expression typed. See chapters/09-analyzer.
package query

import (
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// Node is any bound node. String renders a readable dump; the golden
// tests compare against it.
type Node interface {
	String() string
}

// Stmt is a bound statement.
type Stmt interface {
	Node
	stmtNode()
}

// Expr is a bound expression with a known result type.
type Expr interface {
	Node
	Type() tuple.TypeID
	exprNode()
}

// RangeEntry is one relation of a query's range table.
type RangeEntry struct {
	// Alias is the name the query refers to the relation by: the alias if
	// one was given, else the table name.
	Alias string
	Rel   *catalog.RelationInfo
}

// Select is a bound SELECT. Where and Limit are nil when absent; Where
// includes the ON conditions of every join.
type Select struct {
	Range   []*RangeEntry
	Targets []Target
	Where   Expr
	OrderBy []SortKey
	Limit   Expr
}

// Target is one output column.
type Target struct {
	Name string
	Expr Expr
}

// SortKey is one ORDER BY key, an expression over the range table.
type SortKey struct {
	Expr Expr
	Desc bool
}

// Insert is a bound INSERT. Every row has one expression per column of the
// relation, in column order, typed as the column; omitted columns hold a
// NULL constant.
type Insert struct {
	Rel  *RangeEntry
	Rows [][]Expr
}

// Update is a bound UPDATE. Vars in Set and Where refer to range entry 0.
type Update struct {
	Rel   *RangeEntry
	Set   []Assignment
	Where Expr
}

// Assignment sets column Attr (0-based) to Value, typed as the column.
type Assignment struct {
	Attr  int
	Value Expr
}

// Delete is a bound DELETE. Vars in Where refer to range entry 0.
type Delete struct {
	Rel   *RangeEntry
	Where Expr
}

// CreateTable is a bound CREATE TABLE. PrimaryKey is the 0-based index of
// the PRIMARY KEY column, or -1.
type CreateTable struct {
	Name       string
	Desc       *tuple.Desc
	PrimaryKey int
}

// DropTable is a bound DROP TABLE.
type DropTable struct {
	Name string
}

// Begin is BEGIN.
type Begin struct{}

// Commit is COMMIT.
type Commit struct{}

// Rollback is ROLLBACK.
type Rollback struct{}

// Explain is EXPLAIN stmt.
type Explain struct {
	Stmt Stmt
}

func (*Select) stmtNode()      {}
func (*Insert) stmtNode()      {}
func (*Update) stmtNode()      {}
func (*Delete) stmtNode()      {}
func (*CreateTable) stmtNode() {}
func (*DropTable) stmtNode()   {}
func (*Begin) stmtNode()       {}
func (*Commit) stmtNode()      {}
func (*Rollback) stmtNode()    {}
func (*Explain) stmtNode()     {}

// Var is a column of a range table entry: Rel indexes the range table and
// Attr the entry's Desc.Attrs, both 0-based. Alias and Column are copied
// from the range entry for display.
type Var struct {
	Rel    int
	Attr   int
	Typ    tuple.TypeID
	Alias  string
	Column string
}

// Const is a typed constant. Value is int32, int64, bool, or string
// according to Typ, and nil when Null is set.
type Const struct {
	Typ   tuple.TypeID
	Value tuple.Datum
	Null  bool
}

// OpExpr is an arithmetic or comparison operator applied to two operands
// of the same type. Typ is the operand type for arithmetic and Bool for
// comparisons.
type OpExpr struct {
	Op    ast.BinOp
	Typ   tuple.TypeID
	Left  Expr
	Right Expr
}

// BoolOp is a logical operator.
type BoolOp int

const (
	And BoolOp = iota + 1
	Or
	Not
)

// String returns the operator as written in SQL.
func (o BoolOp) String() string {
	panic("not implemented")
}

// BoolExpr is AND or OR over two or more boolean arguments, or NOT over
// one. Nested applications of the same operator are flattened.
type BoolExpr struct {
	Op   BoolOp
	Args []Expr
}

// Neg is unary minus over an integer operand.
type Neg struct {
	X Expr
}

// NullTest is X IS [NOT] NULL.
type NullTest struct {
	X   Expr
	Not bool
}

// Cast widens an int4 expression to int8, the only implicit conversion.
type Cast struct {
	X   Expr
	Typ tuple.TypeID
}

func (e *Var) Type() tuple.TypeID      { return e.Typ }
func (e *Const) Type() tuple.TypeID    { return e.Typ }
func (e *OpExpr) Type() tuple.TypeID   { return e.Typ }
func (e *BoolExpr) Type() tuple.TypeID { return tuple.Bool }
func (e *Neg) Type() tuple.TypeID      { return e.X.Type() }
func (e *NullTest) Type() tuple.TypeID { return tuple.Bool }
func (e *Cast) Type() tuple.TypeID     { return e.Typ }

func (*Var) exprNode()      {}
func (*Const) exprNode()    {}
func (*OpExpr) exprNode()   {}
func (*BoolExpr) exprNode() {}
func (*Neg) exprNode()      {}
func (*NullTest) exprNode() {}
func (*Cast) exprNode()     {}

// The String methods render the dump format of the chapter README ("The
// dump format"): two-space indentation, no trailing newline, identifiers
// through ast.QuoteIdent, absent clauses omitted, nested statements
// indented by one more level.
func (s *Select) String() string      { panic("not implemented") }
func (s *Insert) String() string      { panic("not implemented") }
func (s *Update) String() string      { panic("not implemented") }
func (s *Delete) String() string      { panic("not implemented") }
func (s *CreateTable) String() string { panic("not implemented") }
func (s *DropTable) String() string   { panic("not implemented") }
func (*Begin) String() string         { panic("not implemented") }
func (*Commit) String() string        { panic("not implemented") }
func (*Rollback) String() string      { panic("not implemented") }
func (s *Explain) String() string     { panic("not implemented") }

// Expressions print as SQL with every operator application in
// parentheses; constants show their type where the digits alone would
// not (1::int8, NULL::bool), so the dump doubles as a type check.
func (e *Var) String() string      { panic("not implemented") }
func (e *Const) String() string    { panic("not implemented") }
func (e *OpExpr) String() string   { panic("not implemented") }
func (e *BoolExpr) String() string { panic("not implemented") }
func (e *Neg) String() string      { panic("not implemented") }
func (e *NullTest) String() string { panic("not implemented") }
func (e *Cast) String() string     { panic("not implemented") }
