package executor

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/executor/expr"
	"github.com/raphi011/build-postgres/internal/heap"
	"github.com/raphi011/build-postgres/internal/index"
	"github.com/raphi011/build-postgres/internal/plan"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/query"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// db is a bootstrapped data directory with one table t (a int4 not null,
// b text, c bool).
type db struct {
	env *Env
	cat *catalog.Catalog
	t   *query.RangeEntry
}

var tDesc = tuple.NewDesc(
	tuple.Attr{Name: "a", Type: tuple.Int4, NotNull: true},
	tuple.Attr{Name: "b", Type: tuple.Text},
	tuple.Attr{Name: "c", Type: tuple.Bool},
)

func newDB(t *testing.T) *db {
	t.Helper()
	store, err := smgr.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	pool := bufmgr.New(store, 16)
	cat, err := catalog.Bootstrap(pool)
	if err != nil {
		t.Fatal(err)
	}
	d := &db{env: &Env{Pool: pool, XID: tuple.FrozenXID}, cat: cat}
	d.t = d.create(t, "t", tDesc)
	return d
}

func (d *db) create(t *testing.T, name string, desc *tuple.Desc) *query.RangeEntry {
	t.Helper()
	if _, err := d.cat.CreateTable(name, desc, tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	info, err := d.cat.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return &query.RangeEntry{Alias: name, Rel: info}
}

// seed inserts rows straight into the heap. A nil value is NULL.
func (d *db) seed(t *testing.T, rel *query.RangeEntry, rows ...[]tuple.Datum) []tuple.TID {
	t.Helper()
	h := heap.Open(d.env.Pool, rel.Rel.OID, rel.Rel.Desc)
	var tids []tuple.TID
	for _, r := range rows {
		nulls := make([]bool, len(r))
		for i, v := range r {
			nulls[i] = v == nil
		}
		tup, err := tuple.Form(rel.Rel.Desc, r, nulls)
		if err != nil {
			t.Fatal(err)
		}
		tid, err := h.Insert(tup, tuple.FrozenXID)
		if err != nil {
			t.Fatal(err)
		}
		tids = append(tids, tid)
	}
	return tids
}

// dump runs a plan and renders each row as comma-separated values with
// NULL for nulls, one row per line.
func (d *db) dump(t *testing.T, p plan.Node) string {
	t.Helper()
	rows, _, err := Exec(p, d.env)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return render(rows)
}

func render(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		for i, v := range r.Values {
			if i > 0 {
				b.WriteString(",")
			}
			if r.Nulls[i] {
				b.WriteString("NULL")
			} else {
				fmt.Fprint(&b, v)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Expression helpers over range entry 0.
func col(e *query.RangeEntry, attr int) *query.Var {
	a := e.Rel.Desc.Attrs[attr]
	return &query.Var{Rel: 0, Attr: attr, Typ: a.Type, Alias: e.Alias, Column: a.Name}
}
func i4(n int32) *query.Const   { return &query.Const{Typ: tuple.Int4, Value: n} }
func i8(n int64) *query.Const   { return &query.Const{Typ: tuple.Int8, Value: n} }
func str(s string) *query.Const { return &query.Const{Typ: tuple.Text, Value: s} }
func null(t tuple.TypeID) *query.Const {
	return &query.Const{Typ: t, Null: true}
}
func op(o ast.BinOp, l, r query.Expr) *query.OpExpr {
	typ := tuple.Bool
	switch o {
	case ast.Add, ast.Sub, ast.Mul, ast.Div:
		typ = l.Type()
	}
	return &query.OpExpr{Op: o, Typ: typ, Left: l, Right: r}
}
func targets(exprs ...query.Expr) []query.Target {
	ts := make([]query.Target, len(exprs))
	for i, e := range exprs {
		ts[i] = query.Target{Name: "?column?", Expr: e}
	}
	return ts
}

var threeRows = [][]tuple.Datum{
	{int32(1), "one", true},
	{int32(2), nil, false},
	{int32(3), "three", nil},
}

func TestSeqScan(t *testing.T) {
	d := newDB(t)
	scan := &plan.SeqScan{Rel: d.t}
	if got := d.dump(t, scan); got != "" {
		t.Fatalf("empty table scan = %q", got)
	}
	tids := d.seed(t, d.t, threeRows...)
	rows, n, err := Exec(scan, d.env)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("processed = %d, want 3", n)
	}
	want := "1,one,true\n2,NULL,false\n3,three,NULL\n"
	if got := render(rows); got != want {
		t.Errorf("rows =\n%s\nwant\n%s", got, want)
	}
	for i, r := range rows {
		if r.TID != tids[i] {
			t.Errorf("row %d TID = %v, want %v", i, r.TID, tids[i])
		}
		if len(r.Values) != 3 || len(r.Nulls) != 3 {
			t.Errorf("row %d has %d values, %d nulls", i, len(r.Values), len(r.Nulls))
		}
	}
	// Rows are independent of each other and of the scan's buffers.
	rows[0].Values[0] = int32(99)
	if rows[1].Values[0] != int32(2) {
		t.Error("rows share storage")
	}
}

func TestSeqScanManyPages(t *testing.T) {
	d := newDB(t)
	pad := strings.Repeat("x", 500)
	var rows [][]tuple.Datum
	for i := 0; i < 300; i++ {
		rows = append(rows, []tuple.Datum{int32(i), pad, i%2 == 0})
	}
	d.seed(t, d.t, rows...)
	h := heap.Open(d.env.Pool, d.t.Rel.OID, d.t.Rel.Desc)
	if n, _ := h.NBlocks(); n < 10 {
		t.Fatalf("table has %d blocks, want many", n)
	}
	got, n, err := Exec(&plan.SeqScan{Rel: d.t}, d.env)
	if err != nil {
		t.Fatal(err)
	}
	if n != 300 || len(got) != 300 {
		t.Fatalf("got %d rows (processed %d), want 300", len(got), n)
	}
	for i, r := range got {
		if r.Values[0] != int32(i) {
			t.Fatalf("row %d = %v, want %d", i, r.Values[0], i)
		}
	}
}

func TestNodeLifecycle(t *testing.T) {
	d := newDB(t)
	d.seed(t, d.t, threeRows...)
	n := Build(&plan.SeqScan{Rel: d.t}, d.env)
	if err := n.Open(); err != nil {
		t.Fatal(err)
	}
	var count int
	for {
		_, ok, err := n.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		count++
	}
	if count != 3 {
		t.Errorf("Next returned %d rows, want 3", count)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing a tree that was abandoned after one row must release its
	// buffers: more such scans than the pool has frames still work.
	for i := 0; i < 2*d.env.Pool.NFrames(); i++ {
		n := Build(&plan.SeqScan{Rel: d.t}, d.env)
		if err := n.Open(); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := n.Next(); err != nil || !ok {
			t.Fatalf("Next: ok=%v err=%v", ok, err)
		}
		if err := n.Close(); err != nil {
			t.Fatal(err)
		}
	}
	b, err := d.env.Pool.Pin(d.t.Rel.OID, 0)
	if err != nil {
		t.Fatalf("Pin after abandoned scans: %v", err)
	}
	d.env.Pool.Unpin(b)
}

func TestFilter(t *testing.T) {
	d := newDB(t)
	d.seed(t, d.t, threeRows...)
	scan := &plan.SeqScan{Rel: d.t}
	cases := []struct {
		name string
		qual query.Expr
		want string
	}{
		{"a > 1", op(ast.Gt, col(d.t, 0), i4(1)), "2,NULL,false\n3,three,NULL\n"},
		{"b = 'one'", op(ast.Eq, col(d.t, 1), str("one")), "1,one,true\n"},
		// NULL is not TRUE: the row with a NULL c is filtered out.
		{"c", col(d.t, 2), "1,one,true\n"},
		{"c is null", &query.NullTest{X: col(d.t, 2)}, "3,three,NULL\n"},
		{"false", &query.Const{Typ: tuple.Bool, Value: false}, ""},
		{"null", null(tuple.Bool), ""},
	}
	for _, c := range cases {
		if got := d.dump(t, &plan.Filter{Input: scan, Qual: c.qual}); got != c.want {
			t.Errorf("filter %s =\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
	// An evaluation error stops the query.
	_, _, err := Exec(&plan.Filter{Input: scan, Qual: op(ast.Eq, op(ast.Div, i4(1), i4(0)), i4(0))}, d.env)
	if !errors.Is(err, expr.ErrDivisionByZero) {
		t.Errorf("filter with 1/0: %v, want ErrDivisionByZero", err)
	}
}

func TestProject(t *testing.T) {
	d := newDB(t)
	d.seed(t, d.t, threeRows...)
	scan := &plan.SeqScan{Rel: d.t}
	p := &plan.Project{Input: scan, Targets: targets(
		op(ast.Mul, col(d.t, 0), i4(10)),
		col(d.t, 1),
		str("k"),
		&query.NullTest{X: col(d.t, 2), Not: true},
	)}
	want := "10,one,k,true\n20,NULL,k,true\n30,three,k,false\n"
	if got := d.dump(t, p); got != want {
		t.Errorf("project =\n%s\nwant\n%s", got, want)
	}
	rows, _, _ := Exec(p, d.env)
	if len(rows[0].Values) != 4 {
		t.Errorf("projected row has %d values, want 4", len(rows[0].Values))
	}
	_, _, err := Exec(&plan.Project{Input: scan, Targets: targets(op(ast.Div, col(d.t, 0), i4(0)))}, d.env)
	if !errors.Is(err, expr.ErrDivisionByZero) {
		t.Errorf("project with a/0: %v, want ErrDivisionByZero", err)
	}
}

func TestResult(t *testing.T) {
	d := newDB(t)
	p := &plan.Project{Input: &plan.Result{}, Targets: targets(i4(1), str("x"), null(tuple.Text))}
	if got := d.dump(t, p); got != "1,x,NULL\n" {
		t.Errorf("select 1, 'x', null = %q", got)
	}
}

func TestSort(t *testing.T) {
	d := newDB(t)
	d.seed(t, d.t,
		[]tuple.Datum{int32(3), "b", true},
		[]tuple.Datum{int32(1), nil, false},
		[]tuple.Datum{int32(4), "B", nil},
		[]tuple.Datum{int32(2), "a", true},
		[]tuple.Datum{int32(5), nil, false},
	)
	scan := &plan.SeqScan{Rel: d.t}
	key := func(attr int, desc bool) query.SortKey {
		return query.SortKey{Expr: col(d.t, attr), Desc: desc}
	}
	cases := []struct {
		name string
		keys []query.SortKey
		want string
	}{
		{"a", []query.SortKey{key(0, false)}, "1,NULL,false\n2,a,true\n3,b,true\n4,B,NULL\n5,NULL,false\n"},
		{"a desc", []query.SortKey{key(0, true)}, "5,NULL,false\n4,B,NULL\n3,b,true\n2,a,true\n1,NULL,false\n"},
		// Text is ordered bytewise; NULLs come last ascending and first
		// descending, as in PostgreSQL. Ties are broken by the next key.
		{"b, a", []query.SortKey{key(1, false), key(0, false)}, "4,B,NULL\n2,a,true\n3,b,true\n1,NULL,false\n5,NULL,false\n"},
		{"b desc, a desc", []query.SortKey{key(1, true), key(0, true)}, "5,NULL,false\n1,NULL,false\n3,b,true\n2,a,true\n4,B,NULL\n"},
		{"c, a desc", []query.SortKey{key(2, false), key(0, true)}, "5,NULL,false\n1,NULL,false\n3,b,true\n2,a,true\n4,B,NULL\n"},
		{"expression", []query.SortKey{{Expr: op(ast.Sub, i4(0), col(d.t, 0))}}, "5,NULL,false\n4,B,NULL\n3,b,true\n2,a,true\n1,NULL,false\n"},
	}
	for _, c := range cases {
		if got := d.dump(t, &plan.Sort{Input: scan, Keys: c.keys}); got != c.want {
			t.Errorf("order by %s =\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
	// Sorted rows keep their TIDs, so UPDATE ... ORDER BY would work.
	rows, _, _ := Exec(&plan.Sort{Input: scan, Keys: []query.SortKey{key(0, false)}}, d.env)
	if rows[0].TID == rows[1].TID || rows[0].TID.Off != 2 {
		t.Errorf("sorted TIDs = %v, %v", rows[0].TID, rows[1].TID)
	}
	empty := d.create(t, "e", tDesc)
	if got := d.dump(t, &plan.Sort{Input: &plan.SeqScan{Rel: empty}, Keys: []query.SortKey{key(0, false)}}); got != "" {
		t.Errorf("sort of empty input = %q", got)
	}
}

func TestLimit(t *testing.T) {
	d := newDB(t)
	d.seed(t, d.t, threeRows...)
	scan := &plan.SeqScan{Rel: d.t}
	cases := []struct {
		count query.Expr
		want  string
	}{
		{i8(0), ""},
		{i8(2), "1,one,true\n2,NULL,false\n"},
		{i8(3), "1,one,true\n2,NULL,false\n3,three,NULL\n"},
		{i8(10), "1,one,true\n2,NULL,false\n3,three,NULL\n"},
		{null(tuple.Int8), "1,one,true\n2,NULL,false\n3,three,NULL\n"},
		{op(ast.Add, i8(1), i8(1)), "1,one,true\n2,NULL,false\n"},
	}
	for _, c := range cases {
		if got := d.dump(t, &plan.Limit{Input: scan, Count: c.count}); got != c.want {
			t.Errorf("limit %s =\n%s\nwant\n%s", c.count, got, c.want)
		}
	}
	_, _, err := Exec(&plan.Limit{Input: scan, Count: i8(-1)}, d.env)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrInvalidRowCount) || e.Msg != "LIMIT must not be negative" {
		t.Errorf("limit -1: %v, want ErrInvalidRowCount", err)
	}
	// Limit over Sort: the classic top-N.
	top := &plan.Limit{Input: &plan.Sort{Input: scan, Keys: []query.SortKey{{Expr: col(d.t, 0), Desc: true}}}, Count: i8(1)}
	if got := d.dump(t, top); got != "3,three,NULL\n" {
		t.Errorf("top 1 = %q", got)
	}
}

func TestInsert(t *testing.T) {
	d := newDB(t)
	ins := &plan.ModifyTable{Op: plan.Insert, Rel: d.t, Input: &plan.Values{Rows: [][]query.Expr{
		{i4(1), str("one"), &query.Const{Typ: tuple.Bool, Value: true}},
		{op(ast.Add, i4(1), i4(1)), null(tuple.Text), null(tuple.Bool)},
	}}}
	rows, n, err := Exec(ins, d.env)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || len(rows) != 0 {
		t.Errorf("insert processed %d rows and returned %d, want 2 and 0", n, len(rows))
	}
	if got := d.dump(t, &plan.SeqScan{Rel: d.t}); got != "1,one,true\n2,NULL,NULL\n" {
		t.Errorf("after insert:\n%s", got)
	}
	// A NULL in a NOT NULL column is rejected at execution.
	bad := &plan.ModifyTable{Op: plan.Insert, Rel: d.t, Input: &plan.Values{Rows: [][]query.Expr{
		{null(tuple.Int4), str("x"), null(tuple.Bool)},
	}}}
	_, _, err = Exec(bad, d.env)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrNotNull) {
		t.Fatalf("insert null: %v, want ErrNotNull", err)
	}
	if want := `null value in column "a" of relation "t" violates not-null constraint`; e.Msg != want {
		t.Errorf("message %q, want %q", e.Msg, want)
	}
	// Persisted through the pool.
	if err := d.env.Pool.FlushAll(); err != nil {
		t.Fatal(err)
	}
	d.env.Pool.Discard(d.t.Rel.OID)
	if got := d.dump(t, &plan.SeqScan{Rel: d.t}); got != "1,one,true\n2,NULL,NULL\n" {
		t.Errorf("after flush:\n%s", got)
	}
}

func TestUpdate(t *testing.T) {
	d := newDB(t)
	d.seed(t, d.t, threeRows...)
	scan := &plan.SeqScan{Rel: d.t}
	upd := &plan.ModifyTable{Op: plan.Update, Rel: d.t,
		Input: &plan.Filter{Input: scan, Qual: op(ast.Eq, col(d.t, 0), i4(2))},
		Set:   []query.Assignment{{Attr: 1, Value: str("two")}, {Attr: 2, Value: &query.Const{Typ: tuple.Bool, Value: true}}},
	}
	_, n, err := Exec(upd, d.env)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("update processed %d, want 1", n)
	}
	// The new version is appended; the old one is gone from the scan.
	want := "1,one,true\n3,three,NULL\n2,two,true\n"
	if got := d.dump(t, scan); got != want {
		t.Errorf("after update:\n%s\nwant\n%s", got, want)
	}
	// SET expressions see the old row: a swap works.
	u := d.create(t, "u", tuple.NewDesc(
		tuple.Attr{Name: "x", Type: tuple.Int4, NotNull: true},
		tuple.Attr{Name: "y", Type: tuple.Int4},
	))
	d.seed(t, u, []tuple.Datum{int32(1), int32(2)}, []tuple.Datum{int32(3), nil})
	swap := &plan.ModifyTable{Op: plan.Update, Rel: u, Input: &plan.SeqScan{Rel: u},
		Set: []query.Assignment{{Attr: 0, Value: col(u, 1)}, {Attr: 1, Value: col(u, 0)}}}
	_, n, err = Exec(swap, d.env)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrNotNull) {
		t.Fatalf("swap with NULL into x: %v, want ErrNotNull", err)
	}
	if want := `null value in column "x" of relation "u" violates not-null constraint`; e.Msg != want {
		t.Errorf("message %q, want %q", e.Msg, want)
	}
	// Row 1 was swapped before the error on row 2; there is no rollback
	// until chapter 16.
	if got := d.dump(t, &plan.SeqScan{Rel: u}); got != "3,NULL\n2,1\n" {
		t.Errorf("after failed swap:\n%s", got)
	}
	// Updating nothing.
	none := &plan.ModifyTable{Op: plan.Update, Rel: d.t,
		Input: &plan.Filter{Input: scan, Qual: &query.Const{Typ: tuple.Bool, Value: false}},
		Set:   []query.Assignment{{Attr: 1, Value: str("z")}}}
	if _, n, err = Exec(none, d.env); err != nil || n != 0 {
		t.Errorf("update nothing: n=%d err=%v", n, err)
	}
}

// TestUpdateHalloween: an update that appends new tuple versions to pages
// the scan has not reached yet must not see and update them again.
func TestUpdateHalloween(t *testing.T) {
	d := newDB(t)
	pad := strings.Repeat("x", 1000)
	var rows [][]tuple.Datum
	for i := 0; i < 20; i++ { // several pages, the last one partly free
		rows = append(rows, []tuple.Datum{int32(i), pad, true})
	}
	d.seed(t, d.t, rows...)
	upd := &plan.ModifyTable{Op: plan.Update, Rel: d.t, Input: &plan.SeqScan{Rel: d.t},
		Set: []query.Assignment{{Attr: 0, Value: op(ast.Add, col(d.t, 0), i4(100))}}}
	_, n, err := Exec(upd, d.env)
	if err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Errorf("update processed %d rows, want 20", n)
	}
	got, _, _ := Exec(&plan.SeqScan{Rel: d.t}, d.env)
	seen := map[int32]bool{}
	for _, r := range got {
		a := r.Values[0].(int32)
		if a < 100 || a >= 120 || seen[a] {
			t.Fatalf("row a=%d: updated zero or two times", a)
		}
		seen[a] = true
	}
	if len(seen) != 20 {
		t.Errorf("%d rows after update, want 20", len(seen))
	}
}

func TestDelete(t *testing.T) {
	d := newDB(t)
	d.seed(t, d.t, threeRows...)
	scan := &plan.SeqScan{Rel: d.t}
	del := &plan.ModifyTable{Op: plan.Delete, Rel: d.t,
		Input: &plan.Filter{Input: scan, Qual: op(ast.Eq, col(d.t, 0), i4(2))}}
	_, n, err := Exec(del, d.env)
	if err != nil || n != 1 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
	if got := d.dump(t, scan); got != "1,one,true\n3,three,NULL\n" {
		t.Errorf("after delete:\n%s", got)
	}
	all := &plan.ModifyTable{Op: plan.Delete, Rel: d.t, Input: scan}
	if _, n, err = Exec(all, d.env); err != nil || n != 2 {
		t.Fatalf("delete all: n=%d err=%v", n, err)
	}
	if got := d.dump(t, scan); got != "" {
		t.Errorf("after delete all: %q", got)
	}
	if _, n, err = Exec(all, d.env); err != nil || n != 0 {
		t.Fatalf("delete from empty: n=%d err=%v", n, err)
	}
}

// Indexes (chapter 13).

// index creates an index on column attr of rel and reloads rel.Rel so
// that the executor sees it.
func (d *db) index(t *testing.T, rel *query.RangeEntry, name string, attr int, unique bool) *catalog.IndexInfo {
	t.Helper()
	if _, err := d.cat.CreateIndex(name, rel.Rel.OID, attr, unique, false, tuple.FrozenXID); err != nil {
		t.Fatal(err)
	}
	info, err := d.cat.Lookup(rel.Rel.Name)
	if err != nil {
		t.Fatal(err)
	}
	rel.Rel = info
	idx, err := d.cat.LookupIndex(name)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

// indexTIDs returns the heap TIDs an index holds, in index order, after
// checking its structure.
func (d *db) indexTIDs(t *testing.T, rel *query.RangeEntry, idx *catalog.IndexInfo) []tuple.TID {
	t.Helper()
	tree := index.Open(d.env.Pool, rel.Rel, idx)
	if err := tree.Check(); err != nil {
		t.Fatalf("Check(%s): %v", idx.Name, err)
	}
	s := tree.Scan(nil, nil)
	defer s.Close()
	var tids []tuple.TID
	for s.Next() {
		tids = append(tids, s.TID())
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return tids
}

// sortedTIDs returns the TIDs of the rows of p ordered by column attr,
// NULLs last, ties by TID: what an index over attr must hold.
func (d *db) sortedTIDs(t *testing.T, p plan.Node, attr int) []tuple.TID {
	t.Helper()
	rows, _, err := Exec(p, d.env)
	if err != nil {
		t.Fatal(err)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.Nulls[attr] != b.Nulls[attr]:
			return b.Nulls[attr]
		case !a.Nulls[attr]:
			if c := expr.Compare(a.Values[attr], b.Values[attr]); c != 0 {
				return c < 0
			}
		}
		if rows[i].TID.Block != rows[j].TID.Block {
			return rows[i].TID.Block < rows[j].TID.Block
		}
		return rows[i].TID.Off < rows[j].TID.Off
	})
	tids := make([]tuple.TID, len(rows))
	for i, r := range rows {
		tids[i] = r.TID
	}
	return tids
}

func insertPlan(rel *query.RangeEntry, rows ...[]query.Expr) *plan.ModifyTable {
	return &plan.ModifyTable{Op: plan.Insert, Rel: rel, Input: &plan.Values{Rows: rows}}
}

func TestInsertMaintainsIndexes(t *testing.T) {
	d := newDB(t)
	ia := d.index(t, d.t, "t_a", 0, false)
	ib := d.index(t, d.t, "t_b", 1, false)
	ins := insertPlan(d.t,
		[]query.Expr{i4(3), str("c"), &query.Const{Typ: tuple.Bool, Value: true}},
		[]query.Expr{i4(1), null(tuple.Text), null(tuple.Bool)},
		[]query.Expr{i4(2), str("a"), null(tuple.Bool)},
		[]query.Expr{i4(2), str("b"), null(tuple.Bool)},
	)
	if _, n, err := Exec(ins, d.env); err != nil || n != 4 {
		t.Fatalf("insert: n=%d err=%v", n, err)
	}
	scan := &plan.SeqScan{Rel: d.t}
	if got, want := d.indexTIDs(t, d.t, ia), d.sortedTIDs(t, scan, 0); !reflect.DeepEqual(got, want) {
		t.Errorf("t_a TIDs = %v, want %v", got, want)
	}
	if got, want := d.indexTIDs(t, d.t, ib), d.sortedTIDs(t, scan, 1); !reflect.DeepEqual(got, want) {
		t.Errorf("t_b TIDs = %v, want %v", got, want)
	}
	// Many pages: every row is still found through the index.
	var rows [][]query.Expr
	for i := 0; i < 500; i++ {
		rows = append(rows, []query.Expr{i4(int32(1000 - i)), str(strings.Repeat("x", 300)), null(tuple.Bool)})
	}
	if _, n, err := Exec(insertPlan(d.t, rows...), d.env); err != nil || n != 500 {
		t.Fatalf("insert 500: n=%d err=%v", n, err)
	}
	if got, want := d.indexTIDs(t, d.t, ia), d.sortedTIDs(t, scan, 0); !reflect.DeepEqual(got, want) {
		t.Errorf("t_a after 500 rows: %d TIDs, want %d", len(got), len(want))
	}
}

func TestUpdateMaintainsIndexes(t *testing.T) {
	d := newDB(t)
	ia := d.index(t, d.t, "t_a", 0, false)
	d.seed(t, d.t, threeRows...)
	for _, idx := range d.t.Rel.Indexes {
		if err := index.Build(d.env.Pool, d.t.Rel, idx); err != nil {
			t.Fatal(err)
		}
	}
	scan := &plan.SeqScan{Rel: d.t}
	upd := &plan.ModifyTable{Op: plan.Update, Rel: d.t,
		Input: &plan.Filter{Input: scan, Qual: op(ast.Gt, col(d.t, 0), i4(1))},
		Set:   []query.Assignment{{Attr: 0, Value: op(ast.Add, col(d.t, 0), i4(10))}}}
	if _, n, err := Exec(upd, d.env); err != nil || n != 2 {
		t.Fatalf("update: n=%d err=%v", n, err)
	}
	// The new versions are indexed; the old entries stay behind, pointing
	// at the dead tuples (PostgreSQL leaves them for VACUUM).
	got := d.indexTIDs(t, d.t, ia)
	live := d.sortedTIDs(t, scan, 0)
	if len(got) != 5 {
		t.Fatalf("index has %d entries after update, want 5", len(got))
	}
	if !reflect.DeepEqual(got[:1], live[:1]) || !reflect.DeepEqual(got[3:], live[1:]) {
		t.Errorf("index TIDs %v, live rows in key order %v", got, live)
	}
	h := heap.Open(d.env.Pool, d.t.Rel.OID, d.t.Rel.Desc)
	for _, tid := range got[1:3] {
		tup, err := h.Fetch(tid)
		if err != nil || tup.Xmax() == 0 {
			t.Errorf("entry %v: expected a dead tuple, got xmax %d err %v", tid, tup.Xmax(), err)
		}
	}
}

func TestDeleteLeavesIndexEntries(t *testing.T) {
	d := newDB(t)
	ia := d.index(t, d.t, "t_a", 0, false)
	tids := d.seed(t, d.t, threeRows...)
	if err := index.Build(d.env.Pool, d.t.Rel, ia); err != nil {
		t.Fatal(err)
	}
	del := &plan.ModifyTable{Op: plan.Delete, Rel: d.t,
		Input: &plan.Filter{Input: &plan.SeqScan{Rel: d.t}, Qual: op(ast.Eq, col(d.t, 0), i4(2))}}
	if _, n, err := Exec(del, d.env); err != nil || n != 1 {
		t.Fatalf("delete: n=%d err=%v", n, err)
	}
	if got := d.indexTIDs(t, d.t, ia); !reflect.DeepEqual(got, tids) {
		t.Errorf("index TIDs after delete = %v, want %v", got, tids)
	}
	// The index scan skips the dead tuple.
	is := &plan.IndexScan{Rel: d.t, Index: ia}
	if got := d.dump(t, is); got != "1,one,true\n3,three,NULL\n" {
		t.Errorf("index scan after delete:\n%s", got)
	}
}

func TestUniqueViolation(t *testing.T) {
	d := newDB(t)
	ia := d.index(t, d.t, "t_a_key", 0, true)
	ib := d.index(t, d.t, "t_b", 1, false)
	if _, _, err := Exec(insertPlan(d.t, []query.Expr{i4(1), str("one"), null(tuple.Bool)}), d.env); err != nil {
		t.Fatal(err)
	}
	scan := &plan.SeqScan{Rel: d.t}
	before := d.dump(t, scan)
	entries := len(d.indexTIDs(t, d.t, ia))

	// A duplicate insert fails with PostgreSQL's message and writes
	// nothing: no heap tuple, no index entry in any index.
	_, _, err := Exec(insertPlan(d.t, []query.Expr{i4(1), str("uno"), null(tuple.Bool)}), d.env)
	var e *index.Error
	if !errors.As(err, &e) || !errors.Is(err, index.ErrUniqueViolation) {
		t.Fatalf("duplicate insert: %v, want *index.Error wrapping ErrUniqueViolation", err)
	}
	if want := `duplicate key value violates unique constraint "t_a_key"`; e.Msg != want {
		t.Errorf("message %q, want %q", e.Msg, want)
	}
	if got := d.dump(t, scan); got != before {
		t.Errorf("table changed by a failed insert:\n%s", got)
	}
	if n := len(d.indexTIDs(t, d.t, ia)); n != entries {
		t.Errorf("unique index has %d entries, want %d", n, entries)
	}
	if n := len(d.indexTIDs(t, d.t, ib)); n != entries {
		t.Errorf("other index has %d entries, want %d", n, entries)
	}

	// A multi-row insert keeps the rows before the bad one: there is no
	// rollback until chapter 16.
	_, _, err = Exec(insertPlan(d.t,
		[]query.Expr{i4(2), str("two"), null(tuple.Bool)},
		[]query.Expr{i4(1), str("dup"), null(tuple.Bool)},
		[]query.Expr{i4(3), str("three"), null(tuple.Bool)},
	), d.env)
	if !errors.Is(err, index.ErrUniqueViolation) {
		t.Fatalf("multi-row insert: %v", err)
	}
	if got := d.dump(t, scan); got != "1,one,NULL\n2,two,NULL\n" {
		t.Errorf("after failed multi-row insert:\n%s", got)
	}

	// UPDATE to a taken key fails and changes nothing; to the row's own
	// key it succeeds (the old version is not a conflict).
	upd := func(set query.Expr, where query.Expr) error {
		_, _, err := Exec(&plan.ModifyTable{Op: plan.Update, Rel: d.t,
			Input: &plan.Filter{Input: scan, Qual: where},
			Set:   []query.Assignment{{Attr: 0, Value: set}}}, d.env)
		return err
	}
	if err := upd(i4(1), op(ast.Eq, col(d.t, 0), i4(2))); !errors.Is(err, index.ErrUniqueViolation) {
		t.Fatalf("update to a taken key: %v", err)
	}
	if got := d.dump(t, scan); got != "1,one,NULL\n2,two,NULL\n" {
		t.Errorf("after failed update:\n%s", got)
	}
	if err := upd(i4(2), op(ast.Eq, col(d.t, 0), i4(2))); err != nil {
		t.Fatalf("update to own key: %v", err)
	}
	if err := upd(op(ast.Add, col(d.t, 0), i4(10)), op(ast.Eq, col(d.t, 0), i4(2))); err != nil {
		t.Fatalf("update to a free key: %v", err)
	}
	if got := d.dump(t, scan); got != "1,one,NULL\n12,two,NULL\n" {
		t.Errorf("after updates:\n%s", got)
	}
	// A deleted key is free again; NULL keys never conflict.
	if _, _, err := Exec(&plan.ModifyTable{Op: plan.Delete, Rel: d.t,
		Input: &plan.Filter{Input: scan, Qual: op(ast.Eq, col(d.t, 0), i4(1))}}, d.env); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Exec(insertPlan(d.t, []query.Expr{i4(1), str("again"), null(tuple.Bool)}), d.env); err != nil {
		t.Errorf("reinsert of a deleted key: %v", err)
	}
	u := d.create(t, "u", tuple.NewDesc(tuple.Attr{Name: "x", Type: tuple.Int4}))
	d.index(t, u, "u_x_key", 0, true)
	if _, n, err := Exec(insertPlan(u, []query.Expr{null(tuple.Int4)}, []query.Expr{null(tuple.Int4)}), d.env); err != nil || n != 2 {
		t.Errorf("two NULL keys: n=%d err=%v", n, err)
	}
}

// TestIndexRowTooLarge: a value the heap accepts but the index cannot
// hold is refused before the heap write, so heap and index stay in step.
func TestIndexRowTooLarge(t *testing.T) {
	d := newDB(t)
	ib := d.index(t, d.t, "t_b", 1, false)
	long := str(strings.Repeat("x", 3000))
	_, _, err := Exec(insertPlan(d.t, []query.Expr{i4(1), long, null(tuple.Bool)}), d.env)
	var e *index.Error
	if !errors.As(err, &e) || !errors.Is(err, index.ErrTooLarge) {
		t.Fatalf("oversized insert: %v, want *index.Error wrapping ErrTooLarge", err)
	}
	if want := `index row size 3012 exceeds btree version 4 maximum 2704 for index "t_b"`; e.Msg != want {
		t.Errorf("message %q, want %q", e.Msg, want)
	}
	scan := &plan.SeqScan{Rel: d.t}
	if got := d.dump(t, scan); got != "" {
		t.Errorf("heap after refused insert:\n%s", got)
	}
	if n := len(d.indexTIDs(t, d.t, ib)); n != 0 {
		t.Errorf("index has %d entries after refused insert", n)
	}
	// The same on UPDATE: the old row stays.
	if _, _, err := Exec(insertPlan(d.t, []query.Expr{i4(1), str("short"), null(tuple.Bool)}), d.env); err != nil {
		t.Fatal(err)
	}
	upd := &plan.ModifyTable{Op: plan.Update, Rel: d.t, Input: scan,
		Set: []query.Assignment{{Attr: 1, Value: long}}}
	if _, _, err := Exec(upd, d.env); !errors.Is(err, index.ErrTooLarge) {
		t.Fatalf("oversized update: %v", err)
	}
	if got := d.dump(t, scan); got != "1,short,NULL\n" {
		t.Errorf("heap after refused update:\n%s", got)
	}
	if got, want := d.indexTIDs(t, d.t, ib), d.sortedTIDs(t, scan, 1); !reflect.DeepEqual(got, want) {
		t.Errorf("index TIDs after refused update = %v, want %v", got, want)
	}
}

func TestIndexScan(t *testing.T) {
	d := newDB(t)
	ia := d.index(t, d.t, "t_a", 0, false)
	ib := d.index(t, d.t, "t_b", 1, false)
	ins := insertPlan(d.t,
		[]query.Expr{i4(3), str("c"), &query.Const{Typ: tuple.Bool, Value: true}},
		[]query.Expr{i4(1), null(tuple.Text), null(tuple.Bool)},
		[]query.Expr{i4(2), str("a"), null(tuple.Bool)},
		[]query.Expr{i4(2), str("b"), null(tuple.Bool)},
		[]query.Expr{i4(4), str("a"), null(tuple.Bool)},
	)
	if _, _, err := Exec(ins, d.env); err != nil {
		t.Fatal(err)
	}
	a, b := col(d.t, 0), col(d.t, 1)
	cases := []struct {
		name  string
		index *catalog.IndexInfo
		quals []query.Expr
		want  string
	}{
		{"a = 2", ia, []query.Expr{op(ast.Eq, a, i4(2))}, "2,a,NULL\n2,b,NULL\n"},
		{"a = 9", ia, []query.Expr{op(ast.Eq, a, i4(9))}, ""},
		{"a > 1", ia, []query.Expr{op(ast.Gt, a, i4(1))}, "2,a,NULL\n2,b,NULL\n3,c,true\n4,a,NULL\n"},
		{"a >= 2 and a < 3", ia, []query.Expr{op(ast.Ge, a, i4(2)), op(ast.Lt, a, i4(3))}, "2,a,NULL\n2,b,NULL\n"},
		{"a <= 2", ia, []query.Expr{op(ast.Le, a, i4(2))}, "1,NULL,NULL\n2,a,NULL\n2,b,NULL\n"},
		{"a < 1", ia, []query.Expr{op(ast.Lt, a, i4(1))}, ""},
		// Several bounds on one side tighten each other.
		{"a > 1 and a > 2", ia, []query.Expr{op(ast.Gt, a, i4(1)), op(ast.Gt, a, i4(2))}, "3,c,true\n4,a,NULL\n"},
		{"a >= 3 and a > 2", ia, []query.Expr{op(ast.Ge, a, i4(3)), op(ast.Gt, a, i4(2))}, "3,c,true\n4,a,NULL\n"},
		{"a < 3 and a <= 3", ia, []query.Expr{op(ast.Lt, a, i4(3)), op(ast.Le, a, i4(3))}, "1,NULL,NULL\n2,a,NULL\n2,b,NULL\n"},
		{"a >= 3 and a <= 2", ia, []query.Expr{op(ast.Ge, a, i4(3)), op(ast.Le, a, i4(2))}, ""},
		// The right-hand side is any Var-free expression.
		{"a = 1 + 1", ia, []query.Expr{op(ast.Eq, a, op(ast.Add, i4(1), i4(1)))}, "2,a,NULL\n2,b,NULL\n"},
		// A NULL bound matches nothing.
		{"a = null", ia, []query.Expr{op(ast.Eq, a, null(tuple.Int4))}, ""},
		{"a > null", ia, []query.Expr{op(ast.Gt, a, null(tuple.Int4))}, ""},
		// Text keys.
		{"b = 'a'", ib, []query.Expr{op(ast.Eq, b, str("a"))}, "2,a,NULL\n4,a,NULL\n"},
		{"b > 'a'", ib, []query.Expr{op(ast.Gt, b, str("a"))}, "2,b,NULL\n3,c,true\n"},
		// No quals: the whole index in key order, NULL keys last.
		{"all by a", ia, nil, "1,NULL,NULL\n2,a,NULL\n2,b,NULL\n3,c,true\n4,a,NULL\n"},
		{"all by b", ib, nil, "2,a,NULL\n4,a,NULL\n2,b,NULL\n3,c,true\n1,NULL,NULL\n"},
	}
	for _, c := range cases {
		if got := d.dump(t, &plan.IndexScan{Rel: d.t, Index: c.index, Quals: c.quals}); got != c.want {
			t.Errorf("index scan %s =\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
	// Rows carry their heap TIDs, like a Seq Scan's.
	rows, _, _ := Exec(&plan.IndexScan{Rel: d.t, Index: ia, Quals: []query.Expr{op(ast.Eq, a, i4(3))}}, d.env)
	if len(rows) != 1 || rows[0].TID != (tuple.TID{Block: 0, Off: 1}) {
		t.Errorf("index scan rows = %+v", rows)
	}
	// Filter over an index scan.
	f := &plan.Filter{Input: &plan.IndexScan{Rel: d.t, Index: ia, Quals: []query.Expr{op(ast.Ge, a, i4(2))}},
		Qual: op(ast.Eq, b, str("a"))}
	if got := d.dump(t, f); got != "2,a,NULL\n4,a,NULL\n" {
		t.Errorf("filter over index scan:\n%s", got)
	}
	// An empty index.
	e := d.create(t, "e", tDesc)
	ie := d.index(t, e, "e_a", 0, false)
	if got := d.dump(t, &plan.IndexScan{Rel: e, Index: ie}); got != "" {
		t.Errorf("scan of empty index = %q", got)
	}
	// An evaluation error in a bound stops the scan.
	_, _, err := Exec(&plan.IndexScan{Rel: d.t, Index: ia, Quals: []query.Expr{op(ast.Eq, a, op(ast.Div, i4(1), i4(0)))}}, d.env)
	if !errors.Is(err, expr.ErrDivisionByZero) {
		t.Errorf("bound 1/0: %v, want ErrDivisionByZero", err)
	}
}

// TestUpdateThroughIndexScan: the new tuple versions an UPDATE appends
// get index entries the scan has not reached yet; each row must still be
// updated once.
func TestUpdateThroughIndexScan(t *testing.T) {
	d := newDB(t)
	ia := d.index(t, d.t, "t_a", 0, false)
	var rows [][]query.Expr
	for i := 0; i < 20; i++ {
		rows = append(rows, []query.Expr{i4(int32(i)), str(strings.Repeat("x", 1000)), null(tuple.Bool)})
	}
	if _, _, err := Exec(insertPlan(d.t, rows...), d.env); err != nil {
		t.Fatal(err)
	}
	upd := &plan.ModifyTable{Op: plan.Update, Rel: d.t,
		Input: &plan.IndexScan{Rel: d.t, Index: ia, Quals: []query.Expr{op(ast.Ge, col(d.t, 0), i4(0))}},
		Set:   []query.Assignment{{Attr: 0, Value: op(ast.Add, col(d.t, 0), i4(100))}}}
	_, n, err := Exec(upd, d.env)
	if err != nil {
		t.Fatal(err)
	}
	if n != 20 {
		t.Errorf("update processed %d rows, want 20", n)
	}
	got, _, _ := Exec(&plan.IndexScan{Rel: d.t, Index: ia}, d.env)
	for i, r := range got {
		if a := r.Values[0].(int32); a != int32(100+i) {
			t.Fatalf("row %d: a = %d, want %d", i, a, 100+i)
		}
	}
	if len(got) != 20 {
		t.Errorf("%d rows after update, want 20", len(got))
	}
}
