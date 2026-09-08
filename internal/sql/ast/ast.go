// Package ast defines the syntax tree the parser produces. See
// chapters/07-parser.
package ast

import (
	"strings"

	"github.com/raphi011/build-postgres/internal/sql/lexer"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// Node is any syntax tree node. String renders the node as SQL in a
// canonical form: keywords upper case, every operator application in
// parentheses, identifiers quoted when they would not lex back to the same
// name. The golden tests compare against this form.
type Node interface {
	String() string
}

// Stmt is a statement node.
type Stmt interface {
	Node
	stmtNode()
}

// Expr is an expression node. Pos is where the expression starts, except
// for BinaryExpr and IsNull, where it is the operator.
type Expr interface {
	Node
	Pos() lexer.Pos
	exprNode()
}

// TableExpr is an item of a FROM list: a table reference or a join.
type TableExpr interface {
	Node
	tableExpr()
}

// CreateTable is CREATE TABLE name (columns).
type CreateTable struct {
	Name    string
	Columns []ColumnDef
}

// ColumnDef is one column of a CREATE TABLE.
type ColumnDef struct {
	Name       string
	Type       tuple.TypeID
	NotNull    bool
	PrimaryKey bool
}

// DropTable is DROP TABLE name.
type DropTable struct {
	Name string
}

// Insert is INSERT INTO table [(columns)] VALUES rows. Columns is nil when
// the column list is omitted; ColumnLocs holds the position of each
// column name. Loc is the position of the table name.
type Insert struct {
	Loc        lexer.Pos
	Table      string
	Columns    []string
	ColumnLocs []lexer.Pos
	Rows       [][]Expr
}

// Select is a SELECT statement. From, Where, OrderBy, and Limit are nil
// when the clause is absent.
type Select struct {
	Items   []SelectItem
	From    []TableExpr
	Where   Expr
	OrderBy []OrderItem
	Limit   Expr
}

// SelectItem is one entry of the select list. Expr is *Star for "*".
type SelectItem struct {
	Expr  Expr
	Alias string
}

// OrderItem is one ORDER BY entry.
type OrderItem struct {
	Expr Expr
	Desc bool
}

// TableRef is a table name with an optional alias.
type TableRef struct {
	Loc   lexer.Pos
	Name  string
	Alias string
}

// Join is Left JOIN Right ON On. Chained joins nest to the left.
type Join struct {
	Left  TableExpr
	Right TableExpr
	On    Expr
}

// Update is UPDATE table SET assignments [WHERE where]. Loc is the
// position of the table name.
type Update struct {
	Loc   lexer.Pos
	Table string
	Set   []Assignment
	Where Expr
}

// Assignment is column = value in an UPDATE. Loc is the position of the
// column name.
type Assignment struct {
	Column string
	Value  Expr
	Loc    lexer.Pos
}

// Delete is DELETE FROM table [WHERE where]. Loc is the position of the
// table name.
type Delete struct {
	Loc   lexer.Pos
	Table string
	Where Expr
}

// Begin is BEGIN.
type Begin struct{}

// Commit is COMMIT.
type Commit struct{}

// Rollback is ROLLBACK.
type Rollback struct{}

// Explain is EXPLAIN stmt; Stmt is a Select, Insert, Update, or Delete.
type Explain struct {
	Stmt Stmt
}

func (*CreateTable) stmtNode() {}
func (*DropTable) stmtNode()   {}
func (*Insert) stmtNode()      {}
func (*Select) stmtNode()      {}
func (*Update) stmtNode()      {}
func (*Delete) stmtNode()      {}
func (*Begin) stmtNode()       {}
func (*Commit) stmtNode()      {}
func (*Rollback) stmtNode()    {}
func (*Explain) stmtNode()     {}

func (*TableRef) tableExpr() {}
func (*Join) tableExpr()     {}

// BinOp is a binary operator.
type BinOp int

const (
	Eq BinOp = iota + 1
	Ne
	Lt
	Le
	Gt
	Ge
	Add
	Sub
	Mul
	Div
	And
	Or
)

var binOpNames = [...]string{
	Eq: "=", Ne: "<>", Lt: "<", Le: "<=", Gt: ">", Ge: ">=",
	Add: "+", Sub: "-", Mul: "*", Div: "/", And: "AND", Or: "OR",
}

// String returns the operator as written in SQL.
func (o BinOp) String() string {
	if o < 1 || int(o) >= len(binOpNames) {
		return "BinOp(?)"
	}
	return binOpNames[o]
}

// UnOp is a prefix operator.
type UnOp int

const (
	Neg UnOp = iota + 1 // unary minus
	Not
)

// String returns the operator as written in SQL.
func (o UnOp) String() string {
	switch o {
	case Neg:
		return "-"
	case Not:
		return "NOT"
	}
	return "UnOp(?)"
}

// IntLit is an integer literal. Text holds the digits as written; the
// analyzer decides the type and range.
type IntLit struct {
	Loc  lexer.Pos
	Text string
}

// StrLit is a string literal with the quotes removed and doubled quotes
// collapsed.
type StrLit struct {
	Loc   lexer.Pos
	Value string
}

// BoolLit is TRUE or FALSE.
type BoolLit struct {
	Loc   lexer.Pos
	Value bool
}

// NullLit is NULL.
type NullLit struct {
	Loc lexer.Pos
}

// ColumnRef is a column name, optionally qualified by a table name or
// alias. Table is "" when unqualified.
type ColumnRef struct {
	Loc   lexer.Pos
	Table string
	Name  string
}

// Star is the "*" of a select list.
type Star struct {
	Loc lexer.Pos
}

// BinaryExpr is Left Op Right. Loc is the operator's position.
type BinaryExpr struct {
	Loc   lexer.Pos
	Op    BinOp
	Left  Expr
	Right Expr
}

// UnaryExpr is Op X. Loc is the operator's position.
type UnaryExpr struct {
	Loc lexer.Pos
	Op  UnOp
	X   Expr
}

// IsNull is X IS [NOT] NULL. Loc is the position of IS.
type IsNull struct {
	Loc lexer.Pos
	X   Expr
	Not bool
}

func (e *IntLit) Pos() lexer.Pos     { return e.Loc }
func (e *StrLit) Pos() lexer.Pos     { return e.Loc }
func (e *BoolLit) Pos() lexer.Pos    { return e.Loc }
func (e *NullLit) Pos() lexer.Pos    { return e.Loc }
func (e *ColumnRef) Pos() lexer.Pos  { return e.Loc }
func (e *Star) Pos() lexer.Pos       { return e.Loc }
func (e *BinaryExpr) Pos() lexer.Pos { return e.Loc }
func (e *UnaryExpr) Pos() lexer.Pos  { return e.Loc }
func (e *IsNull) Pos() lexer.Pos     { return e.Loc }

func (*IntLit) exprNode()     {}
func (*StrLit) exprNode()     {}
func (*BoolLit) exprNode()    {}
func (*NullLit) exprNode()    {}
func (*ColumnRef) exprNode()  {}
func (*Star) exprNode()       {}
func (*BinaryExpr) exprNode() {}
func (*UnaryExpr) exprNode()  {}
func (*IsNull) exprNode()     {}

// QuoteIdent returns name as it must be written to lex back to the same
// identifier: bare when it is a lower-case word that is not a keyword,
// double-quoted otherwise. Inside the quotes a " is doubled.
// PostgreSQL: quote_identifier in ruleutils.c.
func QuoteIdent(name string) string {
	if isBareIdent(name) {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func isBareIdent(name string) bool {
	if name == "" || lexer.IsKeyword(name) {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_', r >= 0x80:
		case r >= '0' && r <= '9', r == '$':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// QuoteString returns value as a SQL string literal: single-quoted with
// every ' doubled and nothing else escaped.
// PostgreSQL: simple_quote_literal in ruleutils.c.
func QuoteString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (s *CreateTable) String() string {
	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	b.WriteString(QuoteIdent(s.Name))
	b.WriteString(" (")
	for i, c := range s.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(QuoteIdent(c.Name))
		b.WriteByte(' ')
		b.WriteString(c.Type.String())
		if c.NotNull {
			b.WriteString(" NOT NULL")
		}
		if c.PrimaryKey {
			b.WriteString(" PRIMARY KEY")
		}
	}
	b.WriteByte(')')
	return b.String()
}

func (s *DropTable) String() string {
	return "DROP TABLE " + QuoteIdent(s.Name)
}

func (s *Insert) String() string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(QuoteIdent(s.Table))
	if s.Columns != nil {
		b.WriteString(" (")
		for i, c := range s.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(QuoteIdent(c))
		}
		b.WriteByte(')')
	}
	b.WriteString(" VALUES ")
	for i, row := range s.Rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		writeExprs(&b, row)
		b.WriteByte(')')
	}
	return b.String()
}

func writeExprs(b *strings.Builder, exprs []Expr) {
	for i, e := range exprs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e.String())
	}
}

func (s *Select) String() string {
	var b strings.Builder
	b.WriteString("SELECT ")
	for i, it := range s.Items {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(it.Expr.String())
		if it.Alias != "" {
			b.WriteString(" AS ")
			b.WriteString(QuoteIdent(it.Alias))
		}
	}
	if s.From != nil {
		b.WriteString(" FROM ")
		for i, t := range s.From {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(t.String())
		}
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(s.Where.String())
	}
	if s.OrderBy != nil {
		b.WriteString(" ORDER BY ")
		for i, o := range s.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(o.Expr.String())
			if o.Desc {
				b.WriteString(" DESC")
			}
		}
	}
	if s.Limit != nil {
		b.WriteString(" LIMIT ")
		b.WriteString(s.Limit.String())
	}
	return b.String()
}

func (t *TableRef) String() string {
	if t.Alias == "" {
		return QuoteIdent(t.Name)
	}
	return QuoteIdent(t.Name) + " AS " + QuoteIdent(t.Alias)
}

func (j *Join) String() string {
	return j.Left.String() + " JOIN " + j.Right.String() + " ON " + j.On.String()
}

func (s *Update) String() string {
	var b strings.Builder
	b.WriteString("UPDATE ")
	b.WriteString(QuoteIdent(s.Table))
	b.WriteString(" SET ")
	for i, a := range s.Set {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(QuoteIdent(a.Column))
		b.WriteString(" = ")
		b.WriteString(a.Value.String())
	}
	if s.Where != nil {
		b.WriteString(" WHERE ")
		b.WriteString(s.Where.String())
	}
	return b.String()
}

func (s *Delete) String() string {
	out := "DELETE FROM " + QuoteIdent(s.Table)
	if s.Where != nil {
		out += " WHERE " + s.Where.String()
	}
	return out
}

func (*Begin) String() string    { return "BEGIN" }
func (*Commit) String() string   { return "COMMIT" }
func (*Rollback) String() string { return "ROLLBACK" }

func (s *Explain) String() string {
	return "EXPLAIN " + s.Stmt.String()
}

func (e *IntLit) String() string { return e.Text }
func (e *StrLit) String() string { return QuoteString(e.Value) }

func (e *BoolLit) String() string {
	if e.Value {
		return "TRUE"
	}
	return "FALSE"
}

func (*NullLit) String() string { return "NULL" }

func (e *ColumnRef) String() string {
	if e.Table == "" {
		return QuoteIdent(e.Name)
	}
	return QuoteIdent(e.Table) + "." + QuoteIdent(e.Name)
}

func (*Star) String() string { return "*" }

func (e *BinaryExpr) String() string {
	return "(" + e.Left.String() + " " + e.Op.String() + " " + e.Right.String() + ")"
}

func (e *UnaryExpr) String() string {
	if e.Op == Not {
		return "(NOT " + e.X.String() + ")"
	}
	return "(" + e.Op.String() + e.X.String() + ")"
}

func (e *IsNull) String() string {
	if e.Not {
		return "(" + e.X.String() + " IS NOT NULL)"
	}
	return "(" + e.X.String() + " IS NULL)"
}

// CreateIndex is CREATE [UNIQUE] INDEX name ON table (column). There is
// no position: PostgreSQL reports CREATE INDEX errors without one.
type CreateIndex struct {
	Name   string
	Table  string
	Column string
	Unique bool
}

// DropIndex is DROP INDEX name.
type DropIndex struct {
	Name string
}

func (*CreateIndex) stmtNode() {}
func (*DropIndex) stmtNode()   {}

func (s *CreateIndex) String() string { panic("not implemented") }
func (s *DropIndex) String() string   { panic("not implemented") }
