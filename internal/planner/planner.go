// Package planner turns a bound statement into a plan tree, choosing
// scans, join methods, and the join order by estimated cost. See
// chapters/11-executor, chapters/14-planner, and chapters/15-joins.
package planner

import (
	"errors"
	"math"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/page"
	"github.com/raphi011/build-postgres/internal/plan"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/query"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// ErrUtility is returned for statements the planner does not handle:
// DDL, transaction control, and EXPLAIN, which the session runs itself.
var ErrUtility = errors.New("planner: utility statement")

// ErrTooManyRelations is returned for a query whose FROM list is longer
// than MaxJoinRelations.
var ErrTooManyRelations = errors.New("too many range table entries")

// MaxJoinRelations caps the FROM list, because the join search
// enumerates every subset of it inside every subset: the work grows by
// about 3.5 times per relation, so thirteen already take seconds and
// sixteen are a hang with no way to interrupt it. PostgreSQL switches to
// a genetic search at geqo_threshold, the same number; here the query is
// refused instead.
const MaxJoinRelations = 12

// Cost parameters, PostgreSQL's defaults (costsize.c). A cost of 1 is
// one page read in sequence.
const (
	SeqPageCost       = 1.0
	RandomPageCost    = 4.0
	CPUTupleCost      = 0.01
	CPUIndexTupleCost = 0.005
	CPUOperatorCost   = 0.0025
)

// Selectivities for a column without statistics (selfuncs.h), which is
// every column here: there are no histograms (D21).
const (
	DefaultEqSel        = 0.005     // = and one side of a range
	DefaultIneqSel      = 1.0 / 3.0 // < <= > >= with one bound
	DefaultRangeIneqSel = 0.005     // a lower and an upper bound on one column
	DefaultUnkSel       = 0.005     // IS NULL
	DefaultBoolSel      = 0.5       // a boolean column
)

// DefaultPages is what a never-analysed relation is assumed to hold;
// its tuple count follows from the row width (estimate_rel_size).
const DefaultPages = 10

// DefaultNumDistinct is the number of distinct values assumed for a
// column of a table with at least that many rows, when nothing says
// otherwise (chapter 15, D22); it makes an equality join keep one row
// in 200.
const DefaultNumDistinct = 200

// Plan builds the cheapest plan for a SELECT, INSERT, UPDATE, or DELETE
// under the cost model of chapters 14 and 15. Every relation gets a
// sequential scan and one index scan per index that has a usable qual,
// provides the ORDER BY order, or can take a join qual as a parameter;
// every set of relations gets nested loop and hash join paths over
// every split into two smaller sets; a set keeps the paths that are
// not beaten on startup cost, total cost, order, and parameters; the
// whole plans built from the final set's paths are compared by total
// cost and the first cheapest wins. Every node's Estimate is set.
// Returns ErrTooManyRelations for a FROM list longer than
// MaxJoinRelations; ErrUtility for any other statement kind.
// PostgreSQL: standard_planner in planner.c, make_one_rel in allpaths.c,
// standard_join_search in joinrels.c, add_path in pathnode.c.
func Plan(q query.Stmt) (plan.Node, error) {
	switch q := q.(type) {
	case *query.Select:
		return planSelect(q)
	case *query.Insert:
		vals := &plan.Values{Rows: q.Rows}
		vals.Est = plan.Estimate{
			TotalCost: float64(len(q.Rows)) * (CPUTupleCost + CPUOperatorCost),
			Rows:      float64(len(q.Rows)),
			Width:     relWidth(q.Rel.Rel),
		}
		return modify(plan.Insert, q.Rel, vals, nil), nil
	case *query.Update:
		return modify(plan.Update, q.Rel, cheapestScan(q.Rel, q.Where), q.Set), nil
	case *query.Delete:
		return modify(plan.Delete, q.Rel, cheapestScan(q.Rel, q.Where), nil), nil
	}
	return nil, ErrUtility
}

// modify wraps input in a ModifyTable, which reports no rows.
func modify(op plan.ModifyOp, rel *query.RangeEntry, input plan.Node, set []query.Assignment) plan.Node {
	in := input.Estimate()
	return &plan.ModifyTable{Op: op, Rel: rel, Input: input, Set: set,
		Est: plan.Estimate{StartupCost: in.StartupCost, TotalCost: in.TotalCost}}
}

func planSelect(q *query.Select) (plan.Node, error) {
	switch len(q.Range) {
	case 0:
		var n plan.Node = result(q.Targets)
		if q.Where != nil {
			n = &plan.Filter{Input: n, Qual: q.Where, Est: n.Estimate()}
		}
		return finish(n, q, true), nil
	case 1:
	default:
		return nil, ErrJoin
	}
	var best plan.Node
	for _, path := range scanPaths(q.Range[0], q.Where, q.OrderBy) {
		n := finish(path.node, q, path.ordered)
		if best == nil || n.Estimate().TotalCost < best.Estimate().TotalCost {
			best = n
		}
	}
	return best, nil
}

// finish adds Sort (unless the input is already ordered), Project, and
// Limit above a scan, with their estimates.
func finish(n plan.Node, q *query.Select, ordered bool) plan.Node {
	if len(q.OrderBy) > 0 && !ordered {
		n = sortNode(n, q.OrderBy)
	}
	n = project(n, q.Targets)
	if q.Limit != nil {
		n = limit(n, q.Limit)
	}
	return n
}

// result is the input of a SELECT without FROM: one row at the cost of
// producing a tuple.
func result(targets []query.Target) *plan.Result {
	return &plan.Result{Est: plan.Estimate{TotalCost: CPUTupleCost, Rows: 1, Width: targetsWidth(targets)}}
}

// project adds the target list's evaluation cost per row and sets the
// output width.
// PostgreSQL: the tlist cost in create_projection_path (createplan.c).
func project(input plan.Node, targets []query.Target) plan.Node {
	in := input.Estimate()
	ops := 0
	for _, t := range targets {
		ops += opCount(t.Expr)
	}
	return &plan.Project{Input: input, Targets: targets, Est: plan.Estimate{
		StartupCost: in.StartupCost,
		TotalCost:   in.TotalCost + in.Rows*CPUOperatorCost*float64(ops),
		Rows:        in.Rows,
		Width:       targetsWidth(targets),
	}}
}

// sortNode costs an in-memory sort: the input plus N log2 N comparisons
// at twice cpu_operator_cost each before the first row, then
// cpu_operator_cost per row returned.
// PostgreSQL: cost_tuplesort in costsize.c.
func sortNode(input plan.Node, keys []query.SortKey) plan.Node {
	in := input.Estimate()
	n := math.Max(in.Rows, 2)
	startup := in.TotalCost + 2*CPUOperatorCost*n*math.Log2(n)
	return &plan.Sort{Input: input, Keys: keys, Est: plan.Estimate{
		StartupCost: startup,
		TotalCost:   startup + CPUOperatorCost*n,
		Rows:        in.Rows,
		Width:       in.Width,
	}}
}

// limit scales the input's run cost by the fraction of rows returned. A
// constant count is used as is (NULL means all rows); anything else is
// taken as a tenth of the input.
// PostgreSQL: adjust_limit_rows_costs in pathnode.c.
func limit(input plan.Node, count query.Expr) plan.Node {
	in := input.Estimate()
	rows := in.Rows
	switch c := count.(type) {
	case *query.Const:
		if !c.Null {
			rows = math.Max(float64(c.Value.(int64)), 0)
		}
	default:
		rows = clampRows(in.Rows * 0.1)
	}
	rows = math.Min(rows, in.Rows)
	total := in.StartupCost + (in.TotalCost-in.StartupCost)*rows/in.Rows
	return &plan.Limit{Input: input, Count: count, Est: plan.Estimate{
		StartupCost: in.StartupCost,
		TotalCost:   total,
		Rows:        math.Max(rows, 1),
		Width:       in.Width,
	}}
}

// scanPath is one way to read a relation: the scan node with its
// Filter, and whether its rows come out in the ORDER BY order.
type scanPath struct {
	node    plan.Node
	ordered bool
}

// scanPaths lists the sequential scan and every index scan that has an
// index qual or delivers the requested order.
// PostgreSQL: set_plain_rel_pathlist in allpaths.c and
// create_index_paths in indxpath.c.
func scanPaths(rel *query.RangeEntry, where query.Expr, orderBy []query.SortKey) []scanPath {
	quals := conjuncts(where)
	paths := []scanPath{{node: seqScan(rel, quals), ordered: len(orderBy) == 0}}
	for _, idx := range rel.Rel.Indexes {
		indexQuals, rest := splitQuals(quals, idx)
		ordered := len(orderBy) == 0 || providesOrder(idx, orderBy)
		if len(indexQuals) == 0 && !providesOrder(idx, orderBy) {
			continue
		}
		paths = append(paths, scanPath{node: indexScan(rel, idx, indexQuals, rest), ordered: ordered})
	}
	return paths
}

// cheapestScan is the scan an UPDATE or DELETE reads through.
func cheapestScan(rel *query.RangeEntry, where query.Expr) plan.Node {
	var best plan.Node
	for _, p := range scanPaths(rel, where, nil) {
		if best == nil || p.node.Estimate().TotalCost < best.Estimate().TotalCost {
			best = p.node
		}
	}
	return best
}

// conjuncts splits a WHERE clause into its ANDed parts.
func conjuncts(where query.Expr) []query.Expr {
	if where == nil {
		return nil
	}
	if b, ok := where.(*query.BoolExpr); ok && b.Op == query.And {
		return b.Args
	}
	return []query.Expr{where}
}

// conjunction is the inverse of conjuncts.
func conjunction(quals []query.Expr) query.Expr {
	switch len(quals) {
	case 0:
		return nil
	case 1:
		return quals[0]
	}
	return &query.BoolExpr{Op: query.And, Args: quals}
}

// providesOrder reports whether a forward scan of idx yields the ORDER BY
// order: one ascending key that is the indexed column.
func providesOrder(idx *catalog.IndexInfo, orderBy []query.SortKey) bool {
	if len(orderBy) != 1 || orderBy[0].Desc {
		return false
	}
	v, ok := orderBy[0].Expr.(*query.Var)
	return ok && v.Rel == 0 && v.Attr == idx.Attr
}

// splitQuals separates the quals an index scan on idx can use (a
// comparison between the indexed column and an expression without
// Vars, rewritten with the column on the left) from the rest.
// PostgreSQL: match_clause_to_indexcol in indxpath.c.
func splitQuals(quals []query.Expr, idx *catalog.IndexInfo) (indexQuals, rest []query.Expr) {
	for _, q := range quals {
		if iq := indexQual(q, idx); iq != nil {
			indexQuals = append(indexQuals, iq)
		} else {
			rest = append(rest, q)
		}
	}
	return indexQuals, rest
}

func indexQual(q query.Expr, idx *catalog.IndexInfo) query.Expr {
	op, ok := q.(*query.OpExpr)
	if !ok {
		return nil
	}
	var flipped ast.BinOp
	switch op.Op {
	case ast.Eq:
		flipped = ast.Eq
	case ast.Lt:
		flipped = ast.Gt
	case ast.Le:
		flipped = ast.Ge
	case ast.Gt:
		flipped = ast.Lt
	case ast.Ge:
		flipped = ast.Le
	default:
		return nil
	}
	if isIndexVar(op.Left, idx) && !hasVar(op.Right) {
		return op
	}
	if isIndexVar(op.Right, idx) && !hasVar(op.Left) {
		return &query.OpExpr{Op: flipped, Typ: op.Typ, Left: op.Right, Right: op.Left}
	}
	return nil
}

func isIndexVar(e query.Expr, idx *catalog.IndexInfo) bool {
	v, ok := e.(*query.Var)
	return ok && v.Rel == 0 && v.Attr == idx.Attr
}

// Costing.

// seqScan costs reading every page once and every tuple through the
// quals.
// PostgreSQL: cost_seqscan in costsize.c.
func seqScan(rel *query.RangeEntry, quals []query.Expr) plan.Node {
	pages, tuples := relSize(rel.Rel)
	est := plan.Estimate{
		TotalCost: pages*SeqPageCost + tuples*(CPUTupleCost+CPUOperatorCost*float64(opCount(quals...))),
		Rows:      clampRows(tuples * selectivity(quals)),
		Width:     relWidth(rel.Rel),
	}
	return withFilter(&plan.SeqScan{Rel: rel, Est: est}, conjunction(quals), est)
}

// indexScan costs descending the tree, reading the matching index
// pages and entries, fetching their heap pages (Mackert and Lohman's
// estimate, uncorrelated), and running the remaining quals on the
// fetched tuples.
// PostgreSQL: cost_index in costsize.c, btcostestimate and
// genericcostestimate in selfuncs.c.
func indexScan(rel *query.RangeEntry, idx *catalog.IndexInfo, indexQuals, rest []query.Expr) plan.Node {
	pages, tuples := relSize(rel.Rel)
	indexPages, indexTuples := float64(idx.Pages), float64(idx.Tuples)
	if idx.Pages == 0 {
		indexPages, indexTuples = 1, tuples
	}
	indexSel := selectivity(indexQuals)

	// The index part: descent, then the leaf pages and entries visited.
	descent := 50 * CPUOperatorCost
	if indexTuples > 1 {
		descent += math.Ceil(math.Log2(indexTuples)) * CPUOperatorCost
	}
	entries := indexSel * indexTuples
	indexPagesFetched := 1.0
	if indexPages > 1 && indexTuples > 1 {
		indexPagesFetched = math.Ceil(entries * indexPages / indexTuples)
	}
	indexCost := indexPagesFetched*RandomPageCost + entries*(CPUIndexTupleCost+CPUOperatorCost*float64(opCount(indexQuals...)))

	// The heap part: the pages behind the matching entries, fetched in
	// index order, and the tuples through the remaining quals.
	fetched := clampRows(tuples * indexSel)
	heapPages := pagesFetched(fetched, math.Max(pages, 1))
	heapCost := heapPages*RandomPageCost + fetched*(CPUTupleCost+CPUOperatorCost*float64(opCount(rest...)))

	est := plan.Estimate{
		StartupCost: descent,
		TotalCost:   descent + indexCost + heapCost,
		Rows:        clampRows(tuples * selectivity(indexQuals) * selectivity(rest)),
		Width:       relWidth(rel.Rel),
	}
	return withFilter(&plan.IndexScan{Rel: rel, Index: idx, Quals: indexQuals, Est: est}, conjunction(rest), est)
}

// pagesFetched is Mackert and Lohman's estimate of the distinct heap
// pages touched when fetching n tuples in index order from a relation of
// T pages, for a table that fits in cache: 2TN / (2T + N), at most T.
// PostgreSQL: index_pages_fetched in costsize.c.
func pagesFetched(n, T float64) float64 {
	p := 2 * T * n / (2*T + n)
	if p >= T {
		return T
	}
	return math.Ceil(p)
}

// withFilter wraps a scan in a Filter carrying the same estimate when
// there are quals to apply after it.
func withFilter(scan plan.Node, qual query.Expr, est plan.Estimate) plan.Node {
	if qual == nil {
		return scan
	}
	return &plan.Filter{Input: scan, Qual: qual, Est: est}
}

// relSize is the planner's view of a relation: its statistics, or for a
// never-analysed one DefaultPages pages of rows as wide as its columns.
// PostgreSQL: estimate_rel_size in plancat.c.
func relSize(rel *catalog.RelationInfo) (pages, tuples float64) {
	if rel.Pages > 0 {
		return float64(rel.Pages), float64(rel.Tuples)
	}
	// A page holds (PageSize - header) / (tuple header + line pointer +
	// aligned data) tuples.
	tupleWidth := float64(align8(relWidth(rel)) + 24 + 4)
	density := float64(page.PageSize-page.HeaderSize) / tupleWidth
	return DefaultPages, math.Round(DefaultPages * density)
}

func align8(n int) int { return (n + 7) &^ 7 }

// clampRows rounds a row estimate and keeps it at least one.
// PostgreSQL: clamp_row_est in costsize.c.
func clampRows(rows float64) float64 {
	if rows <= 1 {
		return 1
	}
	return math.RoundToEven(rows)
}

// typeWidth is the planner's byte width of a value: the size of the
// fixed types and 32 for text, PostgreSQL's guess without statistics.
func typeWidth(t tuple.TypeID) int {
	if t == tuple.Text {
		return 32
	}
	return t.Size()
}

func relWidth(rel *catalog.RelationInfo) int {
	w := 0
	for _, a := range rel.Desc.Attrs {
		w += typeWidth(a.Type)
	}
	return w
}

func targetsWidth(targets []query.Target) int {
	w := 0
	for _, t := range targets {
		w += typeWidth(t.Expr.Type())
	}
	return w
}

// opCount counts the operator applications in expressions: what costs
// cpu_operator_cost each to evaluate.
// PostgreSQL: cost_qual_eval in costsize.c.
func opCount(exprs ...query.Expr) int {
	n := 0
	for _, e := range exprs {
		switch x := e.(type) {
		case *query.OpExpr:
			n += 1 + opCount(x.Left, x.Right)
		case *query.BoolExpr:
			n += opCount(x.Args...)
		case *query.Neg:
			n += 1 + opCount(x.X)
		case *query.NullTest:
			n += opCount(x.X)
		case *query.Cast:
			n += 1 + opCount(x.X)
		}
	}
	return n
}

// selectivity estimates the fraction of rows that satisfy every qual,
// with the default selectivities: no column has statistics. Inequalities
// on the same column are paired into a range.
// PostgreSQL: clauselist_selectivity in clausesel.c.
func selectivity(quals []query.Expr) float64 {
	type bounds struct{ lo, hi bool }
	ranges := map[[2]int]*bounds{}
	s := 1.0
	for _, q := range quals {
		switch e := q.(type) {
		case *query.OpExpr:
			switch e.Op {
			case ast.Eq:
				s *= DefaultEqSel
			case ast.Ne:
				s *= 1 - DefaultEqSel
			case ast.Lt, ast.Le, ast.Gt, ast.Ge:
				v, lower, ok := rangeBound(e)
				if !ok {
					s *= DefaultIneqSel
					break
				}
				key := [2]int{v.Rel, v.Attr}
				if ranges[key] == nil {
					ranges[key] = &bounds{}
				}
				if lower {
					ranges[key].lo = true
				} else {
					ranges[key].hi = true
				}
			}
		case *query.NullTest:
			if e.Not {
				s *= 1 - DefaultUnkSel
			} else {
				s *= DefaultUnkSel
			}
		case *query.Const:
			if e.Null || e.Value == false {
				s = 0
			}
		case *query.BoolExpr:
			switch e.Op {
			case query.And:
				s *= selectivity(e.Args)
			case query.Or:
				or := 0.0
				for _, arg := range e.Args {
					a := selectivity([]query.Expr{arg})
					or = or + a - or*a
				}
				s *= or
			case query.Not:
				s *= 1 - selectivity(e.Args)
			}
		default:
			s *= DefaultBoolSel
		}
	}
	for _, b := range ranges {
		if b.lo && b.hi {
			s *= DefaultRangeIneqSel
		} else {
			s *= DefaultIneqSel
		}
	}
	return math.Min(math.Max(s, 0), 1)
}

// rangeBound classifies an inequality with a column on one side and no
// column on the other as a lower or upper bound of that column.
func rangeBound(e *query.OpExpr) (v *query.Var, lower, ok bool) {
	if v, ok := e.Left.(*query.Var); ok && !hasVar(e.Right) {
		return v, e.Op == ast.Gt || e.Op == ast.Ge, true
	}
	if v, ok := e.Right.(*query.Var); ok && !hasVar(e.Left) {
		return v, e.Op == ast.Lt || e.Op == ast.Le, true
	}
	return nil, false, false
}

// hasVar reports whether e references a column.
func hasVar(e query.Expr) bool {
	switch x := e.(type) {
	case *query.Var:
		return true
	case *query.OpExpr:
		return hasVar(x.Left) || hasVar(x.Right)
	case *query.BoolExpr:
		for _, arg := range x.Args {
			if hasVar(arg) {
				return true
			}
		}
	case *query.Neg:
		return hasVar(x.X)
	case *query.NullTest:
		return hasVar(x.X)
	case *query.Cast:
		return hasVar(x.X)
	}
	return false
}
