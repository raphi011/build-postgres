// Package query defines the bound statement tree the analyzer produces:
// tables resolved to catalog entries, columns to range table positions,
// and every expression typed. See chapters/09-analyzer.
package query

import (
	"fmt"
	"strconv"
	"strings"

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
	switch o {
	case And:
		return "AND"
	case Or:
		return "OR"
	case Not:
		return "NOT"
	}
	return "BoolOp(?)"
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
func (s *Select) String() string {
	var b strings.Builder
	b.WriteString("Select")
	if len(s.Range) > 0 {
		b.WriteString("\n  From: ")
		for i, r := range s.Range {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(r.String())
		}
	}
	for _, t := range s.Targets {
		fmt.Fprintf(&b, "\n  Target: %s %s := %s", t.Name, t.Expr.Type(), t.Expr)
	}
	if s.Where != nil {
		fmt.Fprintf(&b, "\n  Where: %s", s.Where)
	}
	if len(s.OrderBy) > 0 {
		b.WriteString("\n  Order: ")
		for i, k := range s.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			dir := "ASC"
			if k.Desc {
				dir = "DESC"
			}
			fmt.Fprintf(&b, "%s %s", k.Expr, dir)
		}
	}
	if s.Limit != nil {
		fmt.Fprintf(&b, "\n  Limit: %s", s.Limit)
	}
	return b.String()
}

// String renders the entry as name (oid) or name AS alias (oid).
func (r *RangeEntry) String() string {
	name := ast.QuoteIdent(r.Rel.Name)
	if r.Alias != r.Rel.Name {
		name += " AS " + ast.QuoteIdent(r.Alias)
	}
	return fmt.Sprintf("%s (%d)", name, r.Rel.OID)
}

func (s *Insert) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Insert %s", s.Rel)
	for _, row := range s.Rows {
		b.WriteString("\n  Row: ")
		for i, e := range row {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(e.String())
		}
	}
	return b.String()
}

func (s *Update) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Update %s", s.Rel)
	for _, a := range s.Set {
		fmt.Fprintf(&b, "\n  Set: %s := %s", ast.QuoteIdent(s.Rel.Rel.Desc.Attrs[a.Attr].Name), a.Value)
	}
	if s.Where != nil {
		fmt.Fprintf(&b, "\n  Where: %s", s.Where)
	}
	return b.String()
}

func (s *Delete) String() string {
	out := "Delete " + s.Rel.String()
	if s.Where != nil {
		out += "\n  Where: " + s.Where.String()
	}
	return out
}

func (s *CreateTable) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CreateTable %s (", ast.QuoteIdent(s.Name))
	for i, a := range s.Desc.Attrs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %s", ast.QuoteIdent(a.Name), a.Type)
		if a.NotNull {
			b.WriteString(" NOT NULL")
		}
		if i == s.PrimaryKey {
			b.WriteString(" PRIMARY KEY")
		}
	}
	b.WriteString(")")
	return b.String()
}

func (s *DropTable) String() string { return "DropTable " + ast.QuoteIdent(s.Name) }
func (*Begin) String() string       { return "Begin" }
func (*Commit) String() string      { return "Commit" }
func (*Rollback) String() string    { return "Rollback" }

func (s *Explain) String() string {
	return "Explain\n  " + strings.ReplaceAll(s.Stmt.String(), "\n", "\n  ")
}

// Expressions print as SQL with every operator application in
// parentheses; constants show their type where the digits alone would
// not (1::int8, NULL::bool), so the dump doubles as a type check.
func (e *Var) String() string {
	return ast.QuoteIdent(e.Alias) + "." + ast.QuoteIdent(e.Column)
}

func (e *Const) String() string {
	if e.Null {
		return "NULL::" + e.Typ.String()
	}
	switch v := e.Value.(type) {
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10) + "::int8"
	case bool:
		if v {
			return "TRUE"
		}
		return "FALSE"
	case string:
		return ast.QuoteString(v)
	}
	return fmt.Sprintf("Const(%v)", e.Value)
}

func (e *OpExpr) String() string {
	return "(" + e.Left.String() + " " + e.Op.String() + " " + e.Right.String() + ")"
}

func (e *BoolExpr) String() string {
	if e.Op == Not {
		return "(NOT " + e.Args[0].String() + ")"
	}
	parts := make([]string, len(e.Args))
	for i, a := range e.Args {
		parts[i] = a.String()
	}
	return "(" + strings.Join(parts, " "+e.Op.String()+" ") + ")"
}

func (e *Neg) String() string  { return "(-" + e.X.String() + ")" }
func (e *Cast) String() string { return e.X.String() + "::" + e.Typ.String() }
func (e *NullTest) String() string {
	if e.Not {
		return "(" + e.X.String() + " IS NOT NULL)"
	}
	return "(" + e.X.String() + " IS NULL)"
}

// CreateIndex is a bound CREATE INDEX: the table resolved and the key
// column as a 0-based attribute number.
type CreateIndex struct {
	Name   string
	Rel    *catalog.RelationInfo
	Attr   int
	Unique bool
}

// DropIndex is a bound DROP INDEX.
type DropIndex struct {
	Name string
}

func (*CreateIndex) stmtNode() {}
func (*DropIndex) stmtNode()   {}

// CreateIndex dumps as "CreateIndex i ON t (16384) (a)", followed by
// " UNIQUE" for a unique index; DropIndex as "DropIndex i".
func (s *CreateIndex) String() string {
	out := fmt.Sprintf("CreateIndex %s ON %s (%d) (%s)", ast.QuoteIdent(s.Name),
		ast.QuoteIdent(s.Rel.Name), s.Rel.OID, ast.QuoteIdent(s.Rel.Desc.Attrs[s.Attr].Name))
	if s.Unique {
		out += " UNIQUE"
	}
	return out
}

func (s *DropIndex) String() string { return "DropIndex " + ast.QuoteIdent(s.Name) }
