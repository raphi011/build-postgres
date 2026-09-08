// Package ast defines the syntax tree the parser produces. See
// chapters/07-parser.
package ast

import (
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

// Begin is BEGIN [ISOLATION LEVEL level].
type Begin struct {
	Isolation Isolation
}

// Isolation is a transaction isolation level. READ UNCOMMITTED parses
// as READ COMMITTED, which is how PostgreSQL treats it.
type Isolation int

const (
	// DefaultIsolation is BEGIN without a level: the session's default,
	// READ COMMITTED.
	DefaultIsolation Isolation = iota
	ReadCommitted
	RepeatableRead
)

func (i Isolation) String() string {
	panic("not implemented")
}

// Commit is COMMIT.
type Commit struct{}

// Rollback is ROLLBACK.
type Rollback struct{}

// Explain is EXPLAIN [(COSTS OFF)] stmt; Stmt is a Select, Insert,
// Update, or Delete. CostsOff is set by (COSTS OFF).
type Explain struct {
	Stmt     Stmt
	CostsOff bool
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

// String returns the operator as written in SQL.
func (o BinOp) String() string {
	panic("not implemented")
}

// UnOp is a prefix operator.
type UnOp int

const (
	Neg UnOp = iota + 1 // unary minus
	Not
)

// String returns the operator as written in SQL.
func (o UnOp) String() string {
	panic("not implemented")
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
	panic("not implemented")
}

// QuoteString returns value as a SQL string literal: single-quoted with
// every ' doubled and nothing else escaped.
// PostgreSQL: simple_quote_literal in ruleutils.c.
func QuoteString(value string) string {
	panic("not implemented")
}

func (s *CreateTable) String() string { panic("not implemented") }
func (s *DropTable) String() string   { panic("not implemented") }
func (s *Insert) String() string      { panic("not implemented") }
func (s *Select) String() string      { panic("not implemented") }
func (t *TableRef) String() string    { panic("not implemented") }
func (j *Join) String() string        { panic("not implemented") }
func (s *Update) String() string      { panic("not implemented") }
func (s *Delete) String() string      { panic("not implemented") }
func (b *Begin) String() string       { panic("not implemented") }
func (*Commit) String() string        { panic("not implemented") }
func (*Rollback) String() string      { panic("not implemented") }
func (s *Explain) String() string     { panic("not implemented") }

func (e *IntLit) String() string     { panic("not implemented") }
func (e *StrLit) String() string     { panic("not implemented") }
func (e *BoolLit) String() string    { panic("not implemented") }
func (*NullLit) String() string      { panic("not implemented") }
func (e *ColumnRef) String() string  { panic("not implemented") }
func (*Star) String() string         { panic("not implemented") }
func (e *BinaryExpr) String() string { panic("not implemented") }
func (e *UnaryExpr) String() string  { panic("not implemented") }
func (e *IsNull) String() string     { panic("not implemented") }

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

// Analyze is ANALYZE [table]. Table is "" for the whole database.
type Analyze struct {
	Table string
}

func (*Analyze) stmtNode() {}

func (s *Analyze) String() string { panic("not implemented") }
