// Package plan defines the plan tree the planner produces and the
// executor runs. See chapters/11-executor.
package plan

import (
	"strings"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/query"
)

// Node is a plan node. Range returns the range entries a node's output
// rows are laid out over, in order (see expr.Layout), or nil for nodes
// whose rows are not columns of a relation. Estimate returns the
// planner's cost estimate, zero for a tree built by hand.
type Node interface {
	Range() []*query.RangeEntry
	Estimate() Estimate
	node()
}

// Estimate is what the planner expects of a node (chapter 14), printed
// by EXPLAIN as (cost=StartupCost..TotalCost rows=Rows width=Width):
// the cost before the first row, the cost of the whole scan, the number
// of rows, and their width in bytes. Costs are in PostgreSQL's units,
// where reading one page sequentially costs 1.
type Estimate struct {
	StartupCost float64
	TotalCost   float64
	Rows        float64
	Width       int
}

// String renders e as EXPLAIN prints it: "cost=0.00..1.04 rows=1
// width=36", the costs with two decimals and the rows without.
func (e Estimate) String() string {
	panic("not implemented")
}

// Result yields one empty row. It is the input of a SELECT without FROM.
type Result struct {
	Est Estimate
}

// Values yields one row per expression list, evaluated against an empty
// row. It is the input of INSERT.
type Values struct {
	Rows [][]query.Expr
	Est  Estimate
}

// SeqScan reads every visible tuple of a relation.
type SeqScan struct {
	Rel *query.RangeEntry
	Est Estimate
}

// Filter passes the rows of Input for which Qual is TRUE.
type Filter struct {
	Input Node
	Qual  query.Expr
	Est   Estimate
}

// Project evaluates Targets against each row of Input.
type Project struct {
	Input   Node
	Targets []query.Target
	Est     Estimate
}

// Sort orders the rows of Input by Keys.
type Sort struct {
	Input Node
	Keys  []query.SortKey
	Est   Estimate
}

// Limit passes the first Count rows of Input. Count is an int8 expression
// without Vars; NULL means no limit.
type Limit struct {
	Input Node
	Count query.Expr
	Est   Estimate
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
	switch o {
	case Insert:
		return "Insert"
	case Update:
		return "Update"
	case Delete:
		return "Delete"
	}
	return "ModifyOp(?)"
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
	Est   Estimate
}

func (*Result) Range() []*query.RangeEntry      { return nil }
func (*Values) Range() []*query.RangeEntry      { return nil }
func (n *SeqScan) Range() []*query.RangeEntry   { return []*query.RangeEntry{n.Rel} }
func (n *Filter) Range() []*query.RangeEntry    { return n.Input.Range() }
func (*Project) Range() []*query.RangeEntry     { return nil }
func (n *Sort) Range() []*query.RangeEntry      { return n.Input.Range() }
func (n *Limit) Range() []*query.RangeEntry     { return n.Input.Range() }
func (*ModifyTable) Range() []*query.RangeEntry { return nil }

func (n *Result) Estimate() Estimate      { return n.Est }
func (n *Values) Estimate() Estimate      { return n.Est }
func (n *SeqScan) Estimate() Estimate     { return n.Est }
func (n *Filter) Estimate() Estimate      { return n.Est }
func (n *Project) Estimate() Estimate     { return n.Est }
func (n *Sort) Estimate() Estimate        { return n.Est }
func (n *Limit) Estimate() Estimate       { return n.Est }
func (n *ModifyTable) Estimate() Estimate { return n.Est }

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
	var lines []string
	explain(n, 0, &lines)
	return lines
}

// explain appends the lines for n at the given nesting level. A node's
// headline is prefixed with "->  " below the root; its properties are
// indented two past the headline text.
func explain(n Node, level int, lines *[]string) {
	head, props, children := describe(n)
	indent := 0
	if level > 0 {
		indent = 2 + 6*(level-1)
		head = "->  " + head
	}
	*lines = append(*lines, strings.Repeat(" ", indent)+head)
	propIndent := strings.Repeat(" ", 6*level+2)
	for _, p := range props {
		*lines = append(*lines, propIndent+p)
	}
	for _, c := range children {
		explain(c, level+1, lines)
	}
}

// describe returns a node's headline, its property lines, and its
// children. Project is transparent; Filter attaches to its input.
func describe(n Node) (head string, props []string, children []Node) {
	switch n := n.(type) {
	case *Result:
		return "Result", nil, nil
	case *Values:
		return `Values Scan on "*VALUES*"`, nil, nil
	case *SeqScan:
		head = "Seq Scan on " + ast.QuoteIdent(n.Rel.Rel.Name)
		if n.Rel.Alias != n.Rel.Rel.Name {
			head += " " + ast.QuoteIdent(n.Rel.Alias)
		}
		return head, nil, nil
	case *Filter:
		head, props, children = describe(n.Input)
		label := "Filter: "
		if _, ok := n.Input.(*Result); ok {
			label = "One-Time Filter: "
		}
		return head, append(props, label+n.Qual.String()), children
	case *Project:
		return describe(n.Input)
	case *Sort:
		keys := make([]string, len(n.Keys))
		for i, k := range n.Keys {
			keys[i] = k.Expr.String()
			if k.Desc {
				keys[i] += " DESC"
			}
		}
		return "Sort", []string{"Sort Key: " + strings.Join(keys, ", ")}, []Node{n.Input}
	case *Limit:
		return "Limit", nil, []Node{n.Input}
	case *ModifyTable:
		return n.Op.String() + " on " + ast.QuoteIdent(n.Rel.Rel.Name), nil, []Node{n.Input}
	case *IndexScan:
		head = "Index Scan using " + ast.QuoteIdent(n.Index.Name) + " on " + ast.QuoteIdent(n.Rel.Rel.Name)
		if n.Rel.Alias != n.Rel.Rel.Name {
			head += " " + ast.QuoteIdent(n.Rel.Alias)
		}
		switch len(n.Quals) {
		case 0:
		case 1:
			props = []string{"Index Cond: " + n.Quals[0].String()}
		default:
			props = []string{"Index Cond: " + (&query.BoolExpr{Op: query.And, Args: n.Quals}).String()}
		}
		return head, props, nil
	}
	return "?", nil, nil
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
	Est   Estimate
}

func (n *IndexScan) Range() []*query.RangeEntry { return []*query.RangeEntry{n.Rel} }
func (n *IndexScan) Estimate() Estimate         { return n.Est }
func (*IndexScan) node()                        {}

// ExplainCosts renders the tree as Explain does, with each headline
// followed by two spaces and the node's Estimate in parentheses, as
// EXPLAIN prints without COSTS OFF. A transparent node (Project, Filter)
// lends its estimate to the headline of its input: the estimate printed
// for a scan is that of the outermost node wrapping it.
// PostgreSQL: ExplainNode in explain.c.
func ExplainCosts(n Node) []string {
	panic("not implemented")
}
