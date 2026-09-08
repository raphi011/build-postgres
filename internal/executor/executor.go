// Package executor runs plan trees with the iterator model: every node
// implements Open, Next, and Close and pulls rows from its input. See
// chapters/11-executor.
package executor

import (
	"errors"
	"fmt"
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
	ErrNotNull         = errors.New("not-null violation") // 23502
	ErrInvalidRowCount = errors.New("invalid row count")  // 2201W
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

// Env is what nodes need from their surroundings.
type Env struct {
	Pool *bufmgr.Pool
	XID  tuple.XID
}

// Build turns a plan tree into an executor tree. It does no I/O. Panics
// on a plan node it does not know, and on a HashJoin whose Inner is not
// a Hash.
// PostgreSQL: ExecInitNode in execProcnode.c.
func Build(p plan.Node, env *Env) Node {
	switch p := p.(type) {
	case *plan.Result:
		return &result{}
	case *plan.Values:
		return &values{rows: p.Rows}
	case *plan.SeqScan:
		return &seqScan{env: env, rel: p.Rel}
	case *plan.IndexScan:
		return &indexScan{env: env, rel: p.Rel, index: p.Index, quals: p.Quals}
	case *plan.Filter:
		return &filter{input: Build(p.Input, env), layout: expr.NewLayout(p.Input.Range()), qual: p.Qual}
	case *plan.Project:
		return &project{input: Build(p.Input, env), layout: expr.NewLayout(p.Input.Range()), targets: p.Targets}
	case *plan.Sort:
		return &sortNode{input: Build(p.Input, env), layout: expr.NewLayout(p.Input.Range()), keys: p.Keys}
	case *plan.Limit:
		return &limit{input: Build(p.Input, env), count: p.Count}
	case *plan.ModifyTable:
		return &modifyTable{env: env, op: p.Op, rel: p.Rel, input: Build(p.Input, env),
			layout: expr.NewLayout(p.Input.Range()), set: p.Set}
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
func (n *result) Close() error { return nil }

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

func (n *values) Close() error { return nil }

// seqScan reads a relation through heap.Scan.
type seqScan struct {
	env  *Env
	rel  *query.RangeEntry
	scan *heap.Scan
}

func (n *seqScan) Open() error {
	n.scan = heap.Open(n.env.Pool, n.rel.Rel.OID, n.rel.Rel.Desc).Scan()
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

func (n *seqScan) Close() error {
	n.scan.Close()
	return nil
}

// indexScan reads a key range of an index and fetches each entry's heap
// tuple, skipping dead ones.
type indexScan struct {
	env   *Env
	rel   *query.RangeEntry
	index *catalog.IndexInfo
	quals []query.Expr
	heap  *heap.Relation
	scan  *btree.Scan
	none  bool // a NULL bound: no row can match
}

func (n *indexScan) Open() error {
	n.none = false
	var lo, hi *btree.Bound
	for _, q := range n.quals {
		op := q.(*query.OpExpr)
		v, null, err := expr.Compile(op.Right, expr.NewLayout(nil))(expr.Row{})
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
	n.heap = heap.Open(n.env.Pool, n.rel.Rel.OID, n.rel.Rel.Desc)
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
	if n.none {
		return Row{}, false, nil
	}
	for n.scan.Next() {
		if len(n.quals) > 0 && n.scan.IsNull() {
			break
		}
		t, err := n.heap.Fetch(n.scan.TID())
		if err != nil {
			return Row{}, false, err
		}
		if t.Xmax() != 0 {
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

func (n *filter) Close() error { return n.input.Close() }

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

func (n *project) Close() error { return n.input.Close() }

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

func (n *sortNode) Close() error { return n.input.Close() }

// limit passes the first count rows and stops pulling after that.
type limit struct {
	input Node
	count query.Expr
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
		n.left = v.(int64)
		if n.left < 0 {
			return &Error{Err: ErrInvalidRowCount, Msg: "LIMIT must not be negative"}
		}
	}
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

func (n *limit) Close() error { return n.input.Close() }

// modifyTable writes its input rows to the relation and keeps its
// indexes in step. It pulls every input row before writing the first
// one, so that an UPDATE never sees the tuple versions it is creating
// (the Halloween problem). The unique check runs before the heap write
// (D20); a delete leaves its index entries behind.
type modifyTable struct {
	env    *Env
	op     plan.ModifyOp
	rel    *query.RangeEntry
	input  Node
	layout expr.Layout
	set    []query.Assignment
	funcs  []expr.Func
	count  int
	done   bool
}

func (n *modifyTable) Open() error {
	exprs := make([]query.Expr, len(n.set))
	for i, a := range n.set {
		exprs[i] = a.Value
	}
	n.funcs = compileAll(exprs, n.layout)
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
		if err := n.write(h, r); err != nil {
			return Row{}, false, err
		}
		n.count++
	}
	return Row{}, false, nil
}

// write applies the operation to one input row.
func (n *modifyTable) write(h *heap.Relation, r Row) error {
	switch n.op {
	case plan.Delete:
		return h.Delete(r.TID, n.env.XID)
	case plan.Update:
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
		if err := index.Check(n.env.Pool, n.rel.Rel, vals, nulls, r.TID); err != nil {
			return err
		}
		tid, err := h.Update(r.TID, t, n.env.XID)
		if err != nil {
			return err
		}
		return index.Insert(n.env.Pool, n.rel.Rel, vals, nulls, tid)
	default:
		t, err := n.form(r.Values, r.Nulls)
		if err != nil {
			return err
		}
		if err := index.Check(n.env.Pool, n.rel.Rel, r.Values, r.Nulls, tuple.TID{}); err != nil {
			return err
		}
		tid, err := h.Insert(t, n.env.XID)
		if err != nil {
			return err
		}
		return index.Insert(n.env.Pool, n.rel.Rel, r.Values, r.Nulls, tid)
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

func (n *modifyTable) Close() error { return n.input.Close() }

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
