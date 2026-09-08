// Package planner turns a bound statement into a plan tree, choosing
// scans, join methods, and the join order by estimated cost. See
// chapters/11-executor, chapters/14-planner, and chapters/15-joins.
package planner

import (
	"errors"
	"math"
	"math/bits"

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
		if len(q.Range) > MaxJoinRelations {
			return nil, ErrTooManyRelations
		}
		return planSelect(q), nil
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

func planSelect(q *query.Select) plan.Node {
	if len(q.Range) == 0 {
		var n plan.Node = result(q.Targets)
		if q.Where != nil {
			n = &plan.Filter{Input: n, Qual: q.Where, Est: n.Estimate()}
		}
		return finish(n, q, true)
	}
	p := newPlanner(q.Range, q.Where, q.OrderBy)
	all := p.search()
	var best plan.Node
	for _, path := range p.paths[all] {
		if path.params != 0 {
			continue
		}
		n := finish(path.node, q, path.ordered)
		if best == nil || n.Estimate().TotalCost < best.Estimate().TotalCost {
			best = n
		}
	}
	return best
}

// finish adds Sort (unless the input is already ordered), Project, and
// Limit above a scan or join, with their estimates.
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

// cheapestScan is the scan an UPDATE or DELETE reads through.
func cheapestScan(rel *query.RangeEntry, where query.Expr) plan.Node {
	p := newPlanner([]*query.RangeEntry{rel}, where, nil)
	var best plan.Node
	for _, path := range p.basePaths(0) {
		if best == nil || path.node.Estimate().TotalCost < best.Estimate().TotalCost {
			best = path.node
		}
	}
	return best
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

// Relation sets and paths.

// relSet is a set of range table indexes, one bit per entry.
type relSet uint64

func single(i int) relSet               { return 1 << i }
func (s relSet) has(i int) bool         { return s&single(i) != 0 }
func (s relSet) subsetOf(t relSet) bool { return s&^t == 0 }
func (s relSet) size() int              { return bits.OnesCount64(uint64(s)) }

// path is one way to produce the rows of a set of relations: the node
// with its estimate, whether its rows come out in the ORDER BY order,
// the relations whose current row it needs from a nested loop above
// (zero for an ordinary path), and the join quals it applies itself
// when it does.
// PostgreSQL: Path in pathnodes.h.
type path struct {
	node     plan.Node
	ordered  bool
	params   relSet
	enforced []query.Expr
}

func (p path) est() plan.Estimate { return p.node.Estimate() }

// planner holds one query's range table, its WHERE conjuncts with the
// relations each refers to, and the paths found for every relation set.
type planner struct {
	rng     []*query.RangeEntry
	orderBy []query.SortKey
	quals   []query.Expr
	qrels   []relSet
	rows    map[relSet]float64 // base relations: rows after their quals
	paths   map[relSet][]path
}

func newPlanner(rng []*query.RangeEntry, where query.Expr, orderBy []query.SortKey) *planner {
	p := &planner{rng: rng, orderBy: orderBy, quals: conjuncts(where),
		rows: map[relSet]float64{}, paths: map[relSet][]path{}}
	for _, q := range p.quals {
		p.qrels = append(p.qrels, exprRels(q))
	}
	return p
}

// search builds the paths of every relation set by size and returns the
// set of all relations.
// PostgreSQL: standard_join_search in joinrels.c.
func (p *planner) search() relSet {
	all := relSet(1)<<len(p.rng) - 1
	for i := range p.rng {
		p.paths[single(i)] = p.basePaths(i)
	}
	for size := 2; size <= len(p.rng); size++ {
		for s := relSet(1); s <= all; s++ {
			if s.size() == size {
				p.joinPaths(s)
			}
		}
	}
	return all
}

// addPath keeps new unless a path already kept is at least as good on
// every count (startup cost, total cost, order, parameters), and drops
// the kept paths new beats that way.
// PostgreSQL: add_path in pathnode.c.
func addPath(paths []path, new path) []path {
	for _, old := range paths {
		if dominates(old, new) {
			return paths
		}
	}
	kept := paths[:0]
	for _, old := range paths {
		if !dominates(new, old) {
			kept = append(kept, old)
		}
	}
	return append(kept, new)
}

// dominates reports whether a is at least as good as b on every count.
func dominates(a, b path) bool {
	ae, be := a.est(), b.est()
	return ae.StartupCost <= be.StartupCost && ae.TotalCost <= be.TotalCost &&
		(a.ordered || !b.ordered) && a.params.subsetOf(b.params)
}

// Base relations.

// basePaths lists the ways to read relation i: the sequential scan,
// the index scans with an index qual or the ORDER BY order (chapter
// 14), and one parameterised index scan per join qual an index can
// take as its bound.
// PostgreSQL: set_plain_rel_pathlist in allpaths.c, create_index_paths
// and match_join_clauses_to_index in indxpath.c.
func (p *planner) basePaths(i int) []path {
	rel := p.rng[i]
	var restrict []query.Expr
	for k, q := range p.quals {
		if p.qrels[k] == single(i) || p.qrels[k] == 0 && i == 0 {
			restrict = append(restrict, q)
		}
	}
	seq := p.seqScan(rel, restrict)
	p.rows[single(i)] = seq.Estimate().Rows
	paths := []path{{node: seq, ordered: len(p.orderBy) == 0}}
	for _, idx := range rel.Rel.Indexes {
		indexQuals, rest := p.splitQuals(restrict, rel, idx)
		ordered := p.providesOrder(rel, idx)
		if len(indexQuals) > 0 || ordered {
			paths = addPath(paths, path{node: p.indexScan(rel, idx, indexQuals, rest), ordered: ordered || len(p.orderBy) == 0})
		}
		for k, q := range p.quals {
			if p.qrels[k].size() < 2 || !p.qrels[k].has(i) {
				continue
			}
			iq := indexQual(q, rel, idx)
			if iq == nil {
				continue
			}
			quals := append(append([]query.Expr(nil), indexQuals...), iq)
			paths = addPath(paths, path{node: p.indexScan(rel, idx, quals, rest),
				ordered: len(p.orderBy) == 0,
				params:  p.qrels[k] &^ single(i), enforced: []query.Expr{q}})
		}
	}
	return paths
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
func (p *planner) providesOrder(rel *query.RangeEntry, idx *catalog.IndexInfo) bool {
	if len(p.orderBy) != 1 || p.orderBy[0].Desc {
		return false
	}
	return isIndexVar(p.orderBy[0].Expr, rel, idx)
}

// splitQuals separates the quals an index scan on idx can use (a
// comparison between the indexed column and an expression without
// Vars, rewritten with the column on the left) from the rest.
// PostgreSQL: match_clause_to_indexcol in indxpath.c.
func (p *planner) splitQuals(quals []query.Expr, rel *query.RangeEntry, idx *catalog.IndexInfo) (indexQuals, rest []query.Expr) {
	for _, q := range quals {
		if iq := indexQual(q, rel, idx); iq != nil {
			indexQuals = append(indexQuals, iq)
		} else {
			rest = append(rest, q)
		}
	}
	return indexQuals, rest
}

// indexQual returns q rewritten as an index qual on idx, with the
// indexed column alone on the left and an expression without that
// relation's columns on the right, or nil when q is not one.
func indexQual(q query.Expr, rel *query.RangeEntry, idx *catalog.IndexInfo) query.Expr {
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
	if isIndexVar(op.Left, rel, idx) && !exprRels(op.Right).has(rel.Index) {
		return op
	}
	if isIndexVar(op.Right, rel, idx) && !exprRels(op.Left).has(rel.Index) {
		return &query.OpExpr{Op: flipped, Typ: op.Typ, Left: op.Right, Right: op.Left}
	}
	return nil
}

func isIndexVar(e query.Expr, rel *query.RangeEntry, idx *catalog.IndexInfo) bool {
	v, ok := e.(*query.Var)
	return ok && v.Rel == rel.Index && v.Attr == idx.Attr
}

// Joins.

// joinPaths builds the paths of relation set s from every split into an
// outer and an inner set: a nested loop over a materialised inner path,
// a nested loop over an inner path parameterised by the outer set, and
// a hash join when an equality joins the two sides. Splits are tried
// in increasing order of the outer set.
// PostgreSQL: make_join_rel in joinrels.c, add_paths_to_joinrel and
// match_unsorted_outer in joinpath.c.
func (p *planner) joinPaths(s relSet) {
	for outer := relSet(1); outer < s; outer++ {
		if !outer.subsetOf(s) {
			continue
		}
		inner := s &^ outer
		var joinQuals []query.Expr
		for k, q := range p.quals {
			if r := p.qrels[k]; r.subsetOf(s) && !r.subsetOf(outer) && !r.subsetOf(inner) {
				joinQuals = append(joinQuals, q)
			}
		}
		for _, o := range p.paths[outer] {
			if o.params != 0 {
				continue
			}
			for _, in := range p.paths[inner] {
				switch {
				case in.params == 0:
					p.paths[s] = addPath(p.paths[s], p.nestLoop(s, o, path{node: materialize(in.node)}, joinQuals))
					if hashQuals, rest := splitHashQuals(joinQuals, outer, inner); len(hashQuals) > 0 {
						p.paths[s] = addPath(p.paths[s], p.hashJoin(s, o, in, hashQuals, rest))
					}
				case in.params.subsetOf(outer):
					var rest []query.Expr
					for _, q := range joinQuals {
						if !contains(in.enforced, q) {
							rest = append(rest, q)
						}
					}
					p.paths[s] = addPath(p.paths[s], p.nestLoop(s, o, in, rest))
				}
			}
		}
	}
}

func contains(exprs []query.Expr, e query.Expr) bool {
	for _, x := range exprs {
		if x == e {
			return true
		}
	}
	return false
}

// splitHashQuals separates the equalities between the two sides, each
// rewritten with the outer side's expression on the left, from the
// other join quals.
// PostgreSQL: select_mergejoin_clauses / hash_inner_and_outer in
// joinpath.c.
func splitHashQuals(joinQuals []query.Expr, outer, inner relSet) (hashQuals, rest []query.Expr) {
	for _, q := range joinQuals {
		op, ok := q.(*query.OpExpr)
		if ok && op.Op == ast.Eq {
			l, r := exprRels(op.Left), exprRels(op.Right)
			switch {
			case l != 0 && r != 0 && l.subsetOf(outer) && r.subsetOf(inner):
				hashQuals = append(hashQuals, op)
				continue
			case l != 0 && r != 0 && l.subsetOf(inner) && r.subsetOf(outer):
				hashQuals = append(hashQuals, &query.OpExpr{Op: ast.Eq, Typ: op.Typ, Left: op.Right, Right: op.Left})
				continue
			}
		}
		rest = append(rest, q)
	}
	return hashQuals, rest
}

// joinRows estimates the rows of relation set s: the product of its
// base relations' rows and of the selectivities of the join quals
// within it.
// PostgreSQL: calc_joinrel_size_estimate in costsize.c.
func (p *planner) joinRows(s relSet) float64 {
	rows := 1.0
	var joinQuals []query.Expr
	for i := range p.rng {
		if s.has(i) {
			rows *= p.rows[single(i)]
		}
	}
	for k, q := range p.quals {
		if p.qrels[k].size() >= 2 && p.qrels[k].subsetOf(s) {
			joinQuals = append(joinQuals, q)
		}
	}
	return clampRows(rows * p.selectivity(joinQuals, -1))
}

// materialize costs storing the input's rows: twice cpu_operator_cost
// per row on top of the input.
// PostgreSQL: cost_material in costsize.c.
func materialize(input plan.Node) plan.Node {
	in := input.Estimate()
	return &plan.Materialize{Input: input, Est: plan.Estimate{
		StartupCost: in.StartupCost,
		TotalCost:   in.TotalCost + 2*CPUOperatorCost*in.Rows,
		Rows:        in.Rows,
		Width:       in.Width,
	}}
}

// nestLoop costs the outer path once and the inner path once per outer
// row: a materialised inner is replayed at cpu_operator_cost per row
// after the first pass, any other inner is run again in full. Each pair
// then costs a tuple plus the quals.
// PostgreSQL: initial_cost_nestloop, final_cost_nestloop, and
// cost_rescan in costsize.c.
func (p *planner) nestLoop(s relSet, outer, inner path, quals []query.Expr) path {
	o, in := outer.est(), inner.est()
	rescanStartup, rescanRun := in.StartupCost, in.TotalCost-in.StartupCost
	if _, ok := inner.node.(*plan.Materialize); ok {
		rescanStartup, rescanRun = 0, CPUOperatorCost*in.Rows
	}
	startup := o.StartupCost + in.StartupCost
	run := o.TotalCost - o.StartupCost + in.TotalCost - in.StartupCost
	if o.Rows > 1 {
		run += (o.Rows - 1) * (rescanStartup + rescanRun)
	}
	run += o.Rows * in.Rows * (CPUTupleCost + CPUOperatorCost*float64(opCount(quals...)))
	node := &plan.NestLoop{Outer: outer.node, Inner: inner.node, Qual: conjunction(quals), Est: plan.Estimate{
		StartupCost: startup,
		TotalCost:   startup + run,
		Rows:        p.joinRows(s),
		Width:       o.Width + in.Width,
	}}
	return path{node: node, ordered: outer.ordered}
}

// hashJoin costs reading the whole inner path into the table before
// the first row, then probing with each outer row: the hash quals on
// the rows of the bucket, taken as half their cost since most buckets
// do not match, and a tuple plus the remaining quals per pair passing
// them.
// PostgreSQL: initial_cost_hashjoin and final_cost_hashjoin in
// costsize.c, ExecChooseHashTableSize in nodeHash.c.
func (p *planner) hashJoin(s relSet, outer, inner path, hashQuals, rest []query.Expr) path {
	o, in := outer.est(), inner.est()
	n := float64(len(hashQuals))
	startup := o.StartupCost + in.TotalCost + (CPUOperatorCost*n+CPUTupleCost)*in.Rows
	run := o.TotalCost - o.StartupCost + CPUOperatorCost*n*o.Rows

	buckets := float64(nextPow2(uint64(math.Max(math.Ceil(in.Rows), 1024))))
	bucketSize := 1.0
	for _, q := range hashQuals {
		bucketSize = math.Min(bucketSize, p.bucketSize(q.(*query.OpExpr).Right, buckets))
	}
	run += CPUOperatorCost * float64(opCount(hashQuals...)) * o.Rows * clampRows(in.Rows*bucketSize) * 0.5
	matched := clampRows(o.Rows * in.Rows * p.selectivity(hashQuals, -1))
	run += matched * (CPUTupleCost + CPUOperatorCost*float64(opCount(rest...)))

	hash := &plan.Hash{Input: inner.node, Est: plan.Estimate{
		StartupCost: in.TotalCost, TotalCost: in.TotalCost, Rows: in.Rows, Width: in.Width}}
	node := &plan.HashJoin{Outer: outer.node, Inner: hash, HashQuals: hashQuals, Qual: conjunction(rest), Est: plan.Estimate{
		StartupCost: startup,
		TotalCost:   startup + run,
		Rows:        p.joinRows(s),
		Width:       o.Width + in.Width,
	}}
	return path{node: node, ordered: len(p.orderBy) == 0}
}

func nextPow2(n uint64) uint64 {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len64(n-1)
}

// bucketSize estimates the fraction of the inner rows that share a hash
// bucket with a given key: one in the number of distinct values of the
// key, scaled down by the fraction of its table the inner side's quals
// keep, and at least one in the number of buckets; a tenth when the
// distinct count is a guess.
// PostgreSQL: estimate_hash_bucket_stats in selfuncs.c.
func (p *planner) bucketSize(key query.Expr, buckets float64) float64 {
	nd, guess := p.ndistinct(key)
	if guess {
		return 0.1
	}
	if v, ok := key.(*query.Var); ok {
		_, tuples := relSize(p.rng[v.Rel].Rel)
		nd = clampRows(nd * p.rows[single(v.Rel)] / tuples)
	}
	return math.Min(math.Max(1/math.Min(nd, buckets), 1e-6), 1)
}

// ndistinct estimates the distinct values of an expression: the table's
// tuples for a column with a unique index, 2 for a boolean column, the
// tuples of the expression's one table when there are fewer than
// DefaultNumDistinct, else DefaultNumDistinct with guess set.
// PostgreSQL: get_variable_numdistinct in selfuncs.c.
func (p *planner) ndistinct(e query.Expr) (nd float64, guess bool) {
	rels := exprRels(e)
	if rels.size() != 1 {
		return DefaultNumDistinct, true
	}
	rel := p.rng[bits.TrailingZeros64(uint64(rels))].Rel
	_, tuples := relSize(rel)
	if v, ok := e.(*query.Var); ok {
		for _, idx := range rel.Indexes {
			if idx.Unique && idx.Attr == v.Attr {
				return tuples, false
			}
		}
		if v.Typ == tuple.Bool {
			return 2, false
		}
	}
	if tuples < DefaultNumDistinct {
		return clampRows(tuples), false
	}
	return DefaultNumDistinct, true
}

// Costing.

// seqScan costs reading every page once and every tuple through the
// quals.
// PostgreSQL: cost_seqscan in costsize.c.
func (p *planner) seqScan(rel *query.RangeEntry, quals []query.Expr) plan.Node {
	pages, tuples := relSize(rel.Rel)
	est := plan.Estimate{
		TotalCost: pages*SeqPageCost + tuples*(CPUTupleCost+CPUOperatorCost*float64(opCount(quals...))),
		Rows:      clampRows(tuples * p.selectivity(quals, rel.Index)),
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
func (p *planner) indexScan(rel *query.RangeEntry, idx *catalog.IndexInfo, indexQuals, rest []query.Expr) plan.Node {
	pages, tuples := relSize(rel.Rel)
	indexPages, indexTuples := float64(idx.Pages), float64(idx.Tuples)
	if idx.Pages == 0 {
		indexPages, indexTuples = 1, tuples
	}
	indexSel := p.selectivity(indexQuals, rel.Index)

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
		Rows:        clampRows(tuples * indexSel * p.selectivity(rest, rel.Index)),
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

// Selectivity.

// selectivity estimates the fraction of rows that satisfy every qual,
// with the default selectivities: no column has statistics. Inequalities
// on the same column are paired into a range. rel is the relation whose
// scan the quals are for, or -1 at a join: an equality between two
// relations' columns is a bound of that relation's scan in the first
// case (one row for a unique column, DefaultEqSel otherwise) and a
// join qual in the second (joinEqSel).
// PostgreSQL: clauselist_selectivity in clausesel.c, eqsel and
// var_eq_non_const in selfuncs.c.
func (p *planner) selectivity(quals []query.Expr, rel int) float64 {
	type bounds struct{ lo, hi bool }
	ranges := map[[2]int]*bounds{}
	s := 1.0
	for _, q := range quals {
		switch e := q.(type) {
		case *query.OpExpr:
			switch e.Op {
			case ast.Eq:
				s *= p.eqSel(e, rel)
			case ast.Ne:
				s *= 1 - p.eqSel(e, rel)
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
				s *= p.selectivity(e.Args, rel)
			case query.Or:
				or := 0.0
				for _, arg := range e.Args {
					a := p.selectivity([]query.Expr{arg}, rel)
					or = or + a - or*a
				}
				s *= or
			case query.Not:
				s *= 1 - p.selectivity(e.Args, rel)
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

// eqSel is the selectivity of an equality: DefaultEqSel within one
// relation; between relations, one row when the relation being scanned
// has a unique index on its side, DefaultEqSel otherwise, and joinEqSel
// at a join.
func (p *planner) eqSel(e *query.OpExpr, rel int) float64 {
	if (exprRels(e.Left) | exprRels(e.Right)).size() < 2 {
		return DefaultEqSel
	}
	if rel < 0 {
		return p.joinEqSel(e)
	}
	for _, side := range []query.Expr{e.Left, e.Right} {
		if v, ok := side.(*query.Var); ok && v.Rel == rel {
			for _, idx := range p.rng[rel].Rel.Indexes {
				if idx.Unique && idx.Attr == v.Attr {
					_, tuples := relSize(p.rng[rel].Rel)
					return 1 / tuples
				}
			}
		}
	}
	return DefaultEqSel
}

// joinEqSel is the selectivity of an equality join: each value on the
// side with fewer distinct values matches one in the other side's
// distinct count, so one row in the larger count survives.
// PostgreSQL: eqjoinsel_inner in selfuncs.c.
func (p *planner) joinEqSel(e *query.OpExpr) float64 {
	l, _ := p.ndistinct(e.Left)
	r, _ := p.ndistinct(e.Right)
	return 1 / math.Max(l, r)
}

// rangeBound classifies an inequality with a column on one side and no
// column on the other as a lower or upper bound of that column.
func rangeBound(e *query.OpExpr) (v *query.Var, lower, ok bool) {
	if v, ok := e.Left.(*query.Var); ok && exprRels(e.Right) == 0 {
		return v, e.Op == ast.Gt || e.Op == ast.Ge, true
	}
	if v, ok := e.Right.(*query.Var); ok && exprRels(e.Left) == 0 {
		return v, e.Op == ast.Lt || e.Op == ast.Le, true
	}
	return nil, false, false
}

// exprRels is the set of relations whose columns e references.
func exprRels(e query.Expr) relSet {
	switch x := e.(type) {
	case *query.Var:
		return single(x.Rel)
	case *query.OpExpr:
		return exprRels(x.Left) | exprRels(x.Right)
	case *query.BoolExpr:
		var s relSet
		for _, arg := range x.Args {
			s |= exprRels(arg)
		}
		return s
	case *query.Neg:
		return exprRels(x.X)
	case *query.NullTest:
		return exprRels(x.X)
	case *query.Cast:
		return exprRels(x.X)
	}
	return 0
}
