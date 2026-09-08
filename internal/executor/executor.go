// Package executor runs plan trees with the iterator model: every node
// implements Open, Next, and Close and pulls rows from its input. See
// chapters/11-executor.
package executor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/raphi011/build-postgres/internal/btree"
	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/executor/expr"
	"github.com/raphi011/build-postgres/internal/heap"
	"github.com/raphi011/build-postgres/internal/index"
	"github.com/raphi011/build-postgres/internal/plan"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/query"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// Sentinel errors, one per PostgreSQL error class raised at execution.
var (
	ErrNotNull              = errors.New("not-null violation")    // 23502
	ErrInvalidRowCount      = errors.New("invalid row count")     // 2201W
	ErrSerializationFailure = errors.New("serialization failure") // 40001
)

// Error is an execution error with PostgreSQL's message text.
type Error struct {
	Err error
	Msg string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// Row is what nodes pass up: the values plus, for rows read from a heap,
// the tuple's TID so that ModifyTable can find it again. A join's rows
// have no TID.
type Row struct {
	expr.Row
	TID tuple.TID
}

// Node is an executor node. Open prepares it, Next returns the next row
// and false when there are no more, Rescan restarts an open node so
// that Next yields its rows from the first again, and Close releases
// what Open took. Next must not be called before Open or after it
// returned false, unless Rescan came between. A node whose rows depend
// on an outer row (a parameterised IndexScan) reads it again on Rescan;
// a node that stores what it built (Materialize, the hash table of a
// HashJoin) keeps it.
// PostgreSQL: ExecProcNode and ExecReScan in execProcnode.c and
// execAmi.c.
type Node interface {
	Open() error
	Next() (Row, bool, error)
	Rescan() error
	Close() error
}

// Env is what nodes need from their surroundings: the pool, the
// transaction's ID for the tuples it writes, the snapshot its scans
// read with (nil for the rule of chapters 11 to 16: every tuple whose
// xmax is zero), and the isolation level, which decides what an UPDATE
// or DELETE does with a row another transaction changed first.
type Env struct {
	Pool      *bufmgr.Pool
	XID       tuple.XID
	Snapshot  heap.Snapshot
	Isolation ast.Isolation
}

// outerRow is the current row of a NestLoop's outer side, which its
// inner side evaluates outer references against: the loop stores each
// outer row here before it rescans the inner side.
// PostgreSQL: the PARAM_EXEC slots a nested loop sets in ExecNestLoop.
type outerRow struct {
	layout expr.Layout
	row    expr.Row
}

// Build turns a plan tree into an executor tree. It does no I/O. Panics
// on a plan node it does not know, and on a HashJoin whose Inner is not
// a Hash.
// PostgreSQL: ExecInitNode in execProcnode.c.
func Build(p plan.Node, env *Env) Node {
	return build(p, env, nil)
}

// build is Build with the outer row a parameterised node reads; nil
// outside a NestLoop's inner side.
func build(p plan.Node, env *Env, outer *outerRow) Node {
	switch p := p.(type) {
	case *plan.Result:
		return &result{}
	case *plan.Values:
		return &values{rows: p.Rows}
	case *plan.SeqScan:
		return &seqScan{env: env, rel: p.Rel}
	case *plan.IndexScan:
		return &indexScan{env: env, rel: p.Rel, index: p.Index, quals: p.Quals, outer: outer}
	case *plan.Filter:
		return &filter{input: build(p.Input, env, outer), layout: expr.NewLayout(p.Input.Range()), qual: p.Qual}
	case *plan.Project:
		return &project{input: build(p.Input, env, outer), layout: expr.NewLayout(p.Input.Range()), targets: p.Targets}
	case *plan.Sort:
		return &sortNode{input: build(p.Input, env, outer), layout: expr.NewLayout(p.Input.Range()), keys: p.Keys}
	case *plan.Limit:
		return &limit{input: build(p.Input, env, outer), count: p.Count}
	case *plan.ModifyTable:
		return &modifyTable{env: env, op: p.Op, rel: p.Rel, input: build(p.Input, env, outer),
			layout: expr.NewLayout(p.Input.Range()), set: p.Set, quals: scanQuals(p.Input)}
	case *plan.NestLoop:
		param := &outerRow{layout: expr.NewLayout(p.Outer.Range())}
		return &nestLoop{outer: build(p.Outer, env, outer), inner: build(p.Inner, env, param), param: param,
			layout: expr.NewLayout(p.Range()), qual: p.Qual}
	case *plan.HashJoin:
		h, ok := build(p.Inner, env, outer).(*hash)
		if !ok {
			panic(fmt.Sprintf("executor: HashJoin inner is %T, want *plan.Hash", p.Inner))
		}
		return &hashJoin{outer: build(p.Outer, env, outer), inner: h, hashQuals: p.HashQuals, qual: p.Qual,
			outerLayout: expr.NewLayout(p.Outer.Range()), innerLayout: expr.NewLayout(p.Inner.Range()),
			layout: expr.NewLayout(p.Range())}
	case *plan.Hash:
		return &hash{input: build(p.Input, env, outer)}
	case *plan.Materialize:
		return &materialize{input: build(p.Input, env, outer)}
	}
	panic(fmt.Sprintf("executor: cannot build %T", p))
}

// Exec runs p to completion: Open, every Next, Close. It returns the
// rows the root produced and the number of rows processed, which is the
// row count for a query and the number of rows written for ModifyTable.
// Close runs even after an error. Errors are the nodes' own: *Error
// wrapping ErrInvalidRowCount for a negative LIMIT, *Error wrapping
// ErrNotNull for a NULL written to a NOT NULL column, and expression
// errors unchanged.
// PostgreSQL: ExecutorStart, ExecutorRun, and ExecutorEnd in execMain.c.
func Exec(p plan.Node, env *Env) (rows []Row, processed int, err error) {
	n := Build(p, env)
	if err := n.Open(); err != nil {
		return nil, 0, err
	}
	defer func() {
		if cerr := n.Close(); err == nil {
			err = cerr
		}
	}()
	for {
		r, ok, err := n.Next()
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			break
		}
		rows = append(rows, r)
	}
	if m, ok := n.(*modifyTable); ok {
		return rows, m.count, nil
	}
	return rows, len(rows), nil
}

// result yields one empty row.
type result struct {
	done bool
}

func (n *result) Open() error { n.done = false; return nil }
func (n *result) Next() (Row, bool, error) {
	if n.done {
		return Row{}, false, nil
	}
	n.done = true
	return Row{}, true, nil
}
func (n *result) Rescan() error { n.done = false; return nil }
func (n *result) Close() error  { return nil }

// values evaluates one expression list per row against an empty row.
type values struct {
	rows  [][]query.Expr
	funcs [][]expr.Func
	pos   int
}

func (n *values) Open() error {
	n.funcs = make([][]expr.Func, len(n.rows))
	for i, row := range n.rows {
		n.funcs[i] = compileAll(row, expr.NewLayout(nil))
	}
	n.pos = 0
	return nil
}

func (n *values) Next() (Row, bool, error) {
	if n.pos >= len(n.funcs) {
		return Row{}, false, nil
	}
	r, err := evalAll(n.funcs[n.pos], expr.Row{})
	n.pos++
	return Row{Row: r}, err == nil, err
}

func (n *values) Rescan() error { n.pos = 0; return nil }
func (n *values) Close() error  { return nil }

// seqScan reads a relation through heap.Scan.
type seqScan struct {
	env  *Env
	rel  *query.RangeEntry
	scan *heap.Scan
}

func (n *seqScan) Open() error {
	n.scan = heap.Open(n.env.Pool, n.rel.Rel.OID, n.rel.Rel.Desc).Scan(n.env.Snapshot)
	return n.scan.Err()
}

func (n *seqScan) Next() (Row, bool, error) {
	if !n.scan.Next() {
		return Row{}, false, n.scan.Err()
	}
	vals, nulls, err := tuple.Deform(n.rel.Rel.Desc, n.scan.Tuple())
	if err != nil {
		return Row{}, false, err
	}
	return Row{Row: expr.Row{Values: vals, Nulls: nulls}, TID: n.scan.TID()}, true, nil
}

func (n *seqScan) Rescan() error {
	n.scan.Close()
	return n.Open()
}

func (n *seqScan) Close() error {
	n.scan.Close()
	return nil
}

// indexScan reads a key range of an index and fetches each entry's heap
// tuple, skipping the ones the snapshot does not see. The bounds are evaluated on the first Next
// after Open or Rescan, against the outer row when there is one.
type indexScan struct {
	env    *Env
	rel    *query.RangeEntry
	index  *catalog.IndexInfo
	quals  []query.Expr
	outer  *outerRow
	bounds []expr.Func
	heap   *heap.Relation
	scan   *btree.Scan
	none   bool // a NULL bound: no row can match
}

func (n *indexScan) Open() error {
	layout := expr.NewLayout(nil)
	if n.outer != nil {
		layout = n.outer.layout
	}
	n.bounds = make([]expr.Func, len(n.quals))
	for i, q := range n.quals {
		n.bounds[i] = expr.Compile(q.(*query.OpExpr).Right, layout)
	}
	n.heap = heap.Open(n.env.Pool, n.rel.Rel.OID, n.rel.Rel.Desc)
	n.scan = nil
	return nil
}

// start evaluates the bounds and opens the tree scan.
func (n *indexScan) start() error {
	n.none = false
	var row expr.Row
	if n.outer != nil {
		row = n.outer.row
	}
	var lo, hi *btree.Bound
	for i, q := range n.quals {
		op := q.(*query.OpExpr)
		v, null, err := n.bounds[i](row)
		if err != nil {
			return err
		}
		if null {
			n.none = true
			return nil
		}
		b := &btree.Bound{Key: v, Inclusive: op.Op == ast.Eq || op.Op == ast.Ge || op.Op == ast.Le}
		switch op.Op {
		case ast.Eq:
			lo, hi = tighten(lo, b, 1), tighten(hi, b, -1)
		case ast.Gt, ast.Ge:
			lo = tighten(lo, b, 1)
		case ast.Lt, ast.Le:
			hi = tighten(hi, b, -1)
		default:
			return fmt.Errorf("executor: index scan cannot use operator %s", op.Op)
		}
	}
	n.scan = index.Open(n.env.Pool, n.rel.Rel, n.index).Scan(lo, hi)
	return nil
}

// tighten merges a new bound into the current one: for a lower bound
// (dir 1) the larger key wins, for an upper bound (dir -1) the smaller;
// at a tie the exclusive bound wins.
func tighten(cur, b *btree.Bound, dir int) *btree.Bound {
	if cur == nil {
		return b
	}
	c := expr.Compare(b.Key, cur.Key) * dir
	if c > 0 || c == 0 && !b.Inclusive {
		return b
	}
	return cur
}

func (n *indexScan) Next() (Row, bool, error) {
	if n.scan == nil && !n.none {
		if err := n.start(); err != nil {
			return Row{}, false, err
		}
	}
	if n.none {
		return Row{}, false, nil
	}
	for n.scan.Next() {
		if len(n.quals) > 0 && n.scan.IsNull() {
			break
		}
		t, visible, err := n.heap.Fetch(n.scan.TID(), n.env.Snapshot)
		if err != nil {
			return Row{}, false, err
		}
		if !visible {
			continue
		}
		vals, nulls, err := tuple.Deform(n.rel.Rel.Desc, t)
		if err != nil {
			return Row{}, false, err
		}
		return Row{Row: expr.Row{Values: vals, Nulls: nulls}, TID: n.scan.TID()}, true, nil
	}
	return Row{}, false, n.scan.Err()
}

func (n *indexScan) Rescan() error {
	if n.scan != nil {
		n.scan.Close()
		n.scan = nil
	}
	n.none = false
	return nil
}

func (n *indexScan) Close() error {
	if n.scan != nil {
		n.scan.Close()
	}
	return nil
}

// filter passes rows whose qual is TRUE.
type filter struct {
	input  Node
	layout expr.Layout
	qual   query.Expr
	f      expr.Func
}

func (n *filter) Open() error {
	n.f = expr.Compile(n.qual, n.layout)
	return n.input.Open()
}

func (n *filter) Next() (Row, bool, error) {
	for {
		r, ok, err := n.input.Next()
		if err != nil || !ok {
			return Row{}, false, err
		}
		pass, err := n.f.Qual(r.Row)
		if err != nil {
			return Row{}, false, err
		}
		if pass {
			return r, true, nil
		}
	}
}

func (n *filter) Rescan() error { return n.input.Rescan() }
func (n *filter) Close() error  { return n.input.Close() }

// project evaluates the target list against each input row.
type project struct {
	input   Node
	layout  expr.Layout
	targets []query.Target
	funcs   []expr.Func
}

func (n *project) Open() error {
	exprs := make([]query.Expr, len(n.targets))
	for i, t := range n.targets {
		exprs[i] = t.Expr
	}
	n.funcs = compileAll(exprs, n.layout)
	return n.input.Open()
}

func (n *project) Next() (Row, bool, error) {
	r, ok, err := n.input.Next()
	if err != nil || !ok {
		return Row{}, false, err
	}
	out, err := evalAll(n.funcs, r.Row)
	if err != nil {
		return Row{}, false, err
	}
	return Row{Row: out, TID: r.TID}, true, nil
}

func (n *project) Rescan() error { return n.input.Rescan() }
func (n *project) Close() error  { return n.input.Close() }

// sortNode materialises its input on the first Next and sorts it.
type sortNode struct {
	input  Node
	layout expr.Layout
	keys   []query.SortKey
	funcs  []expr.Func
	rows   []Row
	sorted bool
	pos    int
}

func (n *sortNode) Open() error {
	exprs := make([]query.Expr, len(n.keys))
	for i, k := range n.keys {
		exprs[i] = k.Expr
	}
	n.funcs = compileAll(exprs, n.layout)
	n.rows, n.sorted, n.pos = nil, false, 0
	return n.input.Open()
}

func (n *sortNode) Next() (Row, bool, error) {
	if !n.sorted {
		if err := n.load(); err != nil {
			return Row{}, false, err
		}
	}
	if n.pos >= len(n.rows) {
		return Row{}, false, nil
	}
	r := n.rows[n.pos]
	n.pos++
	return r, true, nil
}

// load pulls every input row with its key values and sorts.
func (n *sortNode) load() error {
	type keyed struct {
		row  Row
		keys expr.Row
	}
	var rows []keyed
	for {
		r, ok, err := n.input.Next()
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		k, err := evalAll(n.funcs, r.Row)
		if err != nil {
			return err
		}
		rows = append(rows, keyed{r, k})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].keys, rows[j].keys
		for k, key := range n.keys {
			c := compareNullable(a.Values[k], a.Nulls[k], b.Values[k], b.Nulls[k])
			if key.Desc {
				c = -c
			}
			if c != 0 {
				return c < 0
			}
		}
		return false
	})
	n.rows = make([]Row, len(rows))
	for i, r := range rows {
		n.rows[i] = r.row
	}
	n.sorted = true
	return nil
}

// compareNullable orders values with NULL after everything, PostgreSQL's
// default for ascending keys; the caller negates for descending, which
// puts NULL first.
func compareNullable(a tuple.Datum, aNull bool, b tuple.Datum, bNull bool) int {
	switch {
	case aNull && bNull:
		return 0
	case aNull:
		return 1
	case bNull:
		return -1
	}
	return expr.Compare(a, b)
}

func (n *sortNode) Rescan() error {
	n.rows, n.sorted, n.pos = nil, false, 0
	return n.input.Rescan()
}

func (n *sortNode) Close() error { return n.input.Close() }

// limit passes the first count rows and stops pulling after that.
type limit struct {
	input Node
	count query.Expr
	total int64
	left  int64
	all   bool
}

func (n *limit) Open() error {
	v, null, err := expr.Compile(n.count, expr.NewLayout(nil))(expr.Row{})
	if err != nil {
		return err
	}
	n.all = null
	if !null {
		n.total = v.(int64)
		if n.total < 0 {
			return &Error{Err: ErrInvalidRowCount, Msg: "LIMIT must not be negative"}
		}
	}
	n.left = n.total
	return n.input.Open()
}

func (n *limit) Next() (Row, bool, error) {
	if !n.all {
		if n.left == 0 {
			return Row{}, false, nil
		}
		n.left--
	}
	return n.input.Next()
}

func (n *limit) Rescan() error {
	n.left = n.total
	return n.input.Rescan()
}

func (n *limit) Close() error { return n.input.Close() }

// modifyTable writes its input rows to the relation and keeps its
// indexes in step. It pulls every input row before writing the first
// one, so that an UPDATE never sees the tuple versions it is creating
// (the Halloween problem). The unique check runs before the heap write
// (D20); a delete leaves its index entries behind. A row another
// transaction deleted or updated since the scan is skipped or, under
// READ COMMITTED, followed to its newest version and rechecked against
// the statement's quals (EvalPlanQual); under REPEATABLE READ it is a
// serialization failure.
type modifyTable struct {
	env    *Env
	op     plan.ModifyOp
	rel    *query.RangeEntry
	input  Node
	layout expr.Layout
	set    []query.Assignment
	quals  []query.Expr // the input's filter and index quals, for the recheck
	funcs  []expr.Func
	qual   expr.Func // quals ANDed; nil without quals
	count  int
	done   bool
}

// scanQuals collects the quals a scan subtree applies to its rows: the
// Filter quals above it and an IndexScan's own.
func scanQuals(p plan.Node) []query.Expr {
	switch p := p.(type) {
	case *plan.Filter:
		return append([]query.Expr{p.Qual}, scanQuals(p.Input)...)
	case *plan.IndexScan:
		return p.Quals
	}
	return nil
}

func (n *modifyTable) Open() error {
	exprs := make([]query.Expr, len(n.set))
	for i, a := range n.set {
		exprs[i] = a.Value
	}
	n.funcs = compileAll(exprs, n.layout)
	n.qual = nil
	if len(n.quals) > 0 {
		n.qual = expr.Compile(&query.BoolExpr{Op: query.And, Args: n.quals}, n.layout)
	}
	n.count, n.done = 0, false
	return n.input.Open()
}

func (n *modifyTable) Next() (Row, bool, error) {
	if n.done {
		return Row{}, false, nil
	}
	n.done = true
	var rows []Row
	for {
		r, ok, err := n.input.Next()
		if err != nil {
			return Row{}, false, err
		}
		if !ok {
			break
		}
		rows = append(rows, r)
	}
	h := heap.Open(n.env.Pool, n.rel.Rel.OID, n.rel.Rel.Desc)
	for _, r := range rows {
		written, err := n.write(h, r)
		if err != nil {
			return Row{}, false, err
		}
		if written {
			n.count++
		}
	}
	return Row{}, false, nil
}

// write applies the operation to one input row, following a concurrent
// update to the row's newest version under READ COMMITTED. It reports
// whether a row was written.
// PostgreSQL: ExecDelete and ExecUpdate in nodeModifyTable.c.
func (n *modifyTable) write(h *heap.Relation, r Row) (bool, error) {
	for {
		err := n.apply(h, r)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, heap.ErrAlreadyDeleted) || n.env.Snapshot == nil {
			return false, err
		}
		if n.env.Isolation >= ast.RepeatableRead {
			return false, &Error{Err: ErrSerializationFailure, Msg: "could not serialize access due to concurrent update"}
		}
		next, ok, err := n.newest(h, r.TID)
		if err != nil || !ok {
			return false, err
		}
		r = next
	}
}

// newest follows the ctid of the tuple at tid to the version that
// replaced it and returns it as a row if it still satisfies the quals;
// ok is false if the tuple was deleted rather than updated, or the new
// version no longer qualifies.
// PostgreSQL: EvalPlanQual in execMain.c.
func (n *modifyTable) newest(h *heap.Relation, tid tuple.TID) (Row, bool, error) {
	t, _, err := h.Fetch(tid, nil)
	if err != nil {
		return Row{}, false, err
	}
	if t.Ctid() == tid {
		return Row{}, false, nil
	}
	tid = t.Ctid()
	if t, _, err = h.Fetch(tid, nil); err != nil {
		return Row{}, false, err
	}
	vals, nulls, err := tuple.Deform(n.rel.Rel.Desc, t)
	if err != nil {
		return Row{}, false, err
	}
	r := Row{Row: expr.Row{Values: vals, Nulls: nulls}, TID: tid}
	if n.qual != nil {
		pass, err := n.qual.Qual(r.Row)
		if err != nil || !pass {
			return Row{}, false, err
		}
	}
	return r, true, nil
}

// apply performs the operation once on r.
func (n *modifyTable) apply(h *heap.Relation, r Row) error {
	switch n.op {
	case plan.Delete:
		return h.Delete(r.TID, n.env.XID, n.env.Snapshot)
	case plan.Update:
		// Wait for another transaction's change to the row before the
		// unique check, so that the check does not count a new version
		// this update is about to replace.
		if err := h.Lock(r.TID, n.env.XID, n.env.Snapshot); err != nil {
			return err
		}
		newVals, err := evalAll(n.funcs, r.Row)
		if err != nil {
			return err
		}
		vals := append([]tuple.Datum(nil), r.Values...)
		nulls := append([]bool(nil), r.Nulls...)
		for i, a := range n.set {
			vals[a.Attr], nulls[a.Attr] = newVals.Values[i], newVals.Nulls[i]
		}
		t, err := n.form(vals, nulls)
		if err != nil {
			return err
		}
		if err := index.Check(n.env.Pool, n.rel.Rel, vals, nulls, r.TID, n.env.Snapshot); err != nil {
			return err
		}
		tid, err := h.Update(r.TID, t, n.env.XID, n.env.Snapshot)
		if err != nil {
			return err
		}
		return index.Insert(n.env.Pool, n.rel.Rel, vals, nulls, tid, r.TID, n.env.Snapshot)
	default:
		t, err := n.form(r.Values, r.Nulls)
		if err != nil {
			return err
		}
		if err := index.Check(n.env.Pool, n.rel.Rel, r.Values, r.Nulls, tuple.TID{}, n.env.Snapshot); err != nil {
			return err
		}
		tid, err := h.Insert(t, n.env.XID)
		if err != nil {
			return err
		}
		return index.Insert(n.env.Pool, n.rel.Rel, r.Values, r.Nulls, tid, tuple.TID{}, n.env.Snapshot)
	}
}

// form checks NOT NULL constraints and builds the tuple.
func (n *modifyTable) form(vals []tuple.Datum, nulls []bool) (tuple.Tuple, error) {
	desc := n.rel.Rel.Desc
	for i, a := range desc.Attrs {
		if a.NotNull && nulls[i] {
			return nil, &Error{Err: ErrNotNull, Msg: fmt.Sprintf(
				"null value in column %q of relation %q violates not-null constraint", a.Name, n.rel.Rel.Name)}
		}
	}
	return tuple.Form(desc, vals, nulls)
}

func (n *modifyTable) Rescan() error {
	n.count, n.done = 0, false
	return n.input.Rescan()
}

func (n *modifyTable) Close() error { return n.input.Close() }

// Chapter 15: joins.

// nestLoop pairs each outer row with every inner row, rescanning the
// inner side once per outer row with that row as its parameter.
// PostgreSQL: ExecNestLoop in nodeNestloop.c.
type nestLoop struct {
	outer, inner Node
	param        *outerRow
	layout       expr.Layout
	qual         query.Expr
	f            expr.Func
	cur          Row
	haveOuter    bool
}

func (n *nestLoop) Open() error {
	n.f = nil
	if n.qual != nil {
		n.f = expr.Compile(n.qual, n.layout)
	}
	n.haveOuter = false
	if err := n.outer.Open(); err != nil {
		return err
	}
	return n.inner.Open()
}

func (n *nestLoop) Next() (Row, bool, error) {
	for {
		if !n.haveOuter {
			r, ok, err := n.outer.Next()
			if err != nil || !ok {
				return Row{}, false, err
			}
			n.cur, n.haveOuter = r, true
			n.param.row = r.Row
			if err := n.inner.Rescan(); err != nil {
				return Row{}, false, err
			}
		}
		ir, ok, err := n.inner.Next()
		if err != nil {
			return Row{}, false, err
		}
		if !ok {
			n.haveOuter = false
			continue
		}
		row := joinRows(n.cur, ir)
		if n.f != nil {
			pass, err := n.f.Qual(row.Row)
			if err != nil {
				return Row{}, false, err
			}
			if !pass {
				continue
			}
		}
		return row, true, nil
	}
}

func (n *nestLoop) Rescan() error {
	n.haveOuter = false
	return n.outer.Rescan()
}

func (n *nestLoop) Close() error {
	err := n.outer.Close()
	if cerr := n.inner.Close(); err == nil {
		err = cerr
	}
	return err
}

// joinRows is the outer row's columns followed by the inner row's, in a
// fresh row without a TID.
func joinRows(outer, inner Row) Row {
	vals := make([]tuple.Datum, 0, len(outer.Values)+len(inner.Values))
	nulls := make([]bool, 0, len(outer.Nulls)+len(inner.Nulls))
	return Row{Row: expr.Row{
		Values: append(append(vals, outer.Values...), inner.Values...),
		Nulls:  append(append(nulls, outer.Nulls...), inner.Nulls...),
	}}
}

// materialize stores its input's rows on the first pass and replays
// them after a Rescan.
// PostgreSQL: ExecMaterial in nodeMaterial.c.
type materialize struct {
	input  Node
	rows   []Row
	loaded bool
	pos    int
}

func (n *materialize) Open() error {
	n.rows, n.loaded, n.pos = nil, false, 0
	return n.input.Open()
}

func (n *materialize) Next() (Row, bool, error) {
	if !n.loaded {
		for {
			r, ok, err := n.input.Next()
			if err != nil {
				return Row{}, false, err
			}
			if !ok {
				break
			}
			n.rows = append(n.rows, r)
		}
		n.loaded = true
	}
	if n.pos >= len(n.rows) {
		return Row{}, false, nil
	}
	r := n.rows[n.pos]
	n.pos++
	return r, true, nil
}

func (n *materialize) Rescan() error { n.pos = 0; return nil }
func (n *materialize) Close() error  { return n.input.Close() }

// hashTable buckets rows by the hash of their key values; the hash join
// checks the actual equality on the rows of a bucket.
type hashTable map[uint64][]Row

// hash reads its input into the hash table when the join asks for it.
// It yields no rows through Next.
// PostgreSQL: MultiExecHash in nodeHash.c.
type hash struct {
	input Node
}

func (n *hash) Open() error { return n.input.Open() }
func (n *hash) Next() (Row, bool, error) {
	return Row{}, false, errors.New("executor: Hash node does not support Next")
}
func (n *hash) Rescan() error { return n.input.Rescan() }
func (n *hash) Close() error  { return n.input.Close() }

// build reads every input row into a table keyed by the hash of keys;
// rows with a NULL key are left out, since NULL is equal to nothing.
func (n *hash) build(keys []expr.Func) (hashTable, error) {
	table := hashTable{}
	for {
		r, ok, err := n.input.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return table, nil
		}
		h, null, err := hashKeys(keys, r.Row)
		if err != nil {
			return nil, err
		}
		if !null {
			table[h] = append(table[h], r)
		}
	}
}

// hashKeys evaluates the key expressions against r and hashes the
// values together; null reports that one of them is NULL.
func hashKeys(keys []expr.Func, r expr.Row) (h uint64, null bool, err error) {
	f := fnv.New64a()
	var buf [8]byte
	for _, k := range keys {
		v, isNull, err := k(r)
		if err != nil {
			return 0, false, err
		}
		if isNull {
			return 0, true, nil
		}
		switch v := v.(type) {
		case int32:
			binary.LittleEndian.PutUint32(buf[:4], uint32(v))
			f.Write(buf[:4])
		case int64:
			binary.LittleEndian.PutUint64(buf[:8], uint64(v))
			f.Write(buf[:8])
		case bool:
			buf[0] = 0
			if v {
				buf[0] = 1
			}
			f.Write(buf[:1])
		case string:
			f.Write([]byte(v))
			f.Write([]byte{0})
		default:
			return 0, false, fmt.Errorf("executor: cannot hash %T", v)
		}
	}
	return f.Sum64(), false, nil
}

// hashJoin builds the inner side's hash table on the first Next, then
// probes it with each outer row and checks the hash quals and the
// remaining qual on every row of the matching bucket.
// PostgreSQL: ExecHashJoin in nodeHashjoin.c.
type hashJoin struct {
	outer                            Node
	inner                            *hash
	hashQuals                        []query.Expr
	qual                             query.Expr
	outerLayout, innerLayout, layout expr.Layout
	outerKeys                        []expr.Func
	f                                expr.Func // the hash quals and qual, ANDed
	table                            hashTable
	cur                              Row
	bucket                           []Row
	pos                              int
	haveOuter                        bool
}

func (n *hashJoin) Open() error {
	n.outerKeys = make([]expr.Func, len(n.hashQuals))
	for i, q := range n.hashQuals {
		n.outerKeys[i] = expr.Compile(q.(*query.OpExpr).Left, n.outerLayout)
	}
	quals := append([]query.Expr(nil), n.hashQuals...)
	if n.qual != nil {
		quals = append(quals, n.qual)
	}
	n.f = expr.Compile(&query.BoolExpr{Op: query.And, Args: quals}, n.layout)
	n.table, n.haveOuter = nil, false
	if err := n.outer.Open(); err != nil {
		return err
	}
	return n.inner.Open()
}

func (n *hashJoin) Next() (Row, bool, error) {
	if n.table == nil {
		keys := make([]expr.Func, len(n.hashQuals))
		for i, q := range n.hashQuals {
			keys[i] = expr.Compile(q.(*query.OpExpr).Right, n.innerLayout)
		}
		table, err := n.inner.build(keys)
		if err != nil {
			return Row{}, false, err
		}
		n.table = table
	}
	for {
		if !n.haveOuter {
			r, ok, err := n.outer.Next()
			if err != nil || !ok {
				return Row{}, false, err
			}
			h, null, err := hashKeys(n.outerKeys, r.Row)
			if err != nil {
				return Row{}, false, err
			}
			if null {
				continue
			}
			n.cur, n.bucket, n.pos, n.haveOuter = r, n.table[h], 0, true
		}
		if n.pos >= len(n.bucket) {
			n.haveOuter = false
			continue
		}
		row := joinRows(n.cur, n.bucket[n.pos])
		n.pos++
		pass, err := n.f.Qual(row.Row)
		if err != nil {
			return Row{}, false, err
		}
		if pass {
			return row, true, nil
		}
	}
}

// Rescan restarts the outer side and keeps the hash table: the inner
// side does not depend on anything above.
func (n *hashJoin) Rescan() error {
	n.haveOuter = false
	return n.outer.Rescan()
}

func (n *hashJoin) Close() error {
	err := n.outer.Close()
	if cerr := n.inner.Close(); err == nil {
		err = cerr
	}
	return err
}

// compileAll compiles a list of expressions against one layout.
func compileAll(exprs []query.Expr, l expr.Layout) []expr.Func {
	funcs := make([]expr.Func, len(exprs))
	for i, e := range exprs {
		funcs[i] = expr.Compile(e, l)
	}
	return funcs
}

// evalAll evaluates funcs against r into a fresh row.
func evalAll(funcs []expr.Func, r expr.Row) (expr.Row, error) {
	out := expr.Row{Values: make([]tuple.Datum, len(funcs)), Nulls: make([]bool, len(funcs))}
	for i, f := range funcs {
		v, null, err := f(r)
		if err != nil {
			return expr.Row{}, err
		}
		out.Values[i], out.Nulls[i] = v, null
	}
	return out, nil
}
