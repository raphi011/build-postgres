package executor

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/executor/expr"
	"github.com/raphi011/build-postgres/internal/heap"
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
