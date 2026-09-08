// Package plan defines the plan tree the planner produces and the
// executor runs. See chapters/11-executor.
package plan

import (
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/sql/query"
)

// Node is a plan node. Range returns the range entries a node's output
// rows are laid out over, in order (see expr.Layout), or nil for nodes
// whose rows are not columns of a relation.
type Node interface {
	Range() []*query.RangeEntry
	node()
}

// Result yields one empty row. It is the input of a SELECT without FROM.
type Result struct{}

// Values yields one row per expression list, evaluated against an empty
// row. It is the input of INSERT.
type Values struct {
	Rows [][]query.Expr
}

// SeqScan reads every visible tuple of a relation.
type SeqScan struct {
	Rel *query.RangeEntry
}

// Filter passes the rows of Input for which Qual is TRUE.
type Filter struct {
	Input Node
	Qual  query.Expr
}

// Project evaluates Targets against each row of Input.
type Project struct {
	Input   Node
	Targets []query.Target
}

// Sort orders the rows of Input by Keys.
type Sort struct {
	Input Node
	Keys  []query.SortKey
}

// Limit passes the first Count rows of Input. Count is an int8 expression
// without Vars; NULL means no limit.
type Limit struct {
	Input Node
	Count query.Expr
}

// ModifyOp is the kind of a ModifyTable.
type ModifyOp int

const (
	Insert ModifyOp = iota + 1
	Update
	Delete
)

// String returns the operation name as EXPLAIN prints it.
func (o ModifyOp) String() string {
	panic("not implemented")
}

// ModifyTable writes the rows of Input to Rel. For Insert, Input rows are
// full tuples in column order. For Update and Delete, Input rows are the
// relation's own rows with their TIDs; Set is evaluated against the old
// row for Update.
type ModifyTable struct {
	Op    ModifyOp
	Rel   *query.RangeEntry
	Input Node
	Set   []query.Assignment
}

func (*Result) Range() []*query.RangeEntry      { return nil }
func (*Values) Range() []*query.RangeEntry      { return nil }
func (n *SeqScan) Range() []*query.RangeEntry   { return []*query.RangeEntry{n.Rel} }
func (n *Filter) Range() []*query.RangeEntry    { return n.Input.Range() }
func (*Project) Range() []*query.RangeEntry     { return nil }
func (n *Sort) Range() []*query.RangeEntry      { return n.Input.Range() }
func (n *Limit) Range() []*query.RangeEntry     { return n.Input.Range() }
func (*ModifyTable) Range() []*query.RangeEntry { return nil }

func (*Result) node()      {}
func (*Values) node()      {}
func (*SeqScan) node()     {}
func (*Filter) node()      {}
func (*Project) node()     {}
func (*Sort) node()        {}
func (*Limit) node()       {}
func (*ModifyTable) node() {}

// Explain renders the tree in the format of EXPLAIN, one line per
// element, without costs (chapter 14 adds them). Project is invisible;
// Filter is a property line of its input.
// PostgreSQL: ExplainNode in explain.c.
func Explain(n Node) []string {
	panic("not implemented")
}

// IndexScan reads the tuples of Rel whose key in Index satisfies Quals,
// in index order, fetching each from the heap. Every qual is an OpExpr
// with the indexed column as its left operand, an expression without
// Vars on the right, and one of = < <= > >= as operator; several quals
// on the same side tighten each other. No quals reads the whole index.
// EXPLAIN prints "Index Scan using i on t" and the quals ANDed together
// as "Index Cond:".
type IndexScan struct {
	Rel   *query.RangeEntry
	Index *catalog.IndexInfo
	Quals []query.Expr
}

func (n *IndexScan) Range() []*query.RangeEntry { return []*query.RangeEntry{n.Rel} }
func (*IndexScan) node()                        {}
