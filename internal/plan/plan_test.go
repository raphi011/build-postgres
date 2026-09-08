package plan

import (
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/query"
	"github.com/raphi011/build-postgres/internal/tuple"
)

var tInfo = &catalog.RelationInfo{OID: 16384, Name: "t", Kind: catalog.RelKindTable, Desc: tuple.NewDesc(
	tuple.Attr{Name: "a", Type: tuple.Int4},
	tuple.Attr{Name: "b", Type: tuple.Text},
)}

var iInfo = &catalog.IndexInfo{OID: 16385, Name: "i", Rel: 16384, Attr: 0}

var (
	t0   = &query.RangeEntry{Alias: "t", Rel: tInfo}
	tu   = &query.RangeEntry{Alias: "u", Rel: tInfo}
	va   = &query.Var{Rel: 0, Attr: 0, Typ: tuple.Int4, Alias: "t", Column: "a"}
	vb   = &query.Var{Rel: 0, Attr: 1, Typ: tuple.Text, Alias: "t", Column: "b"}
	ua   = &query.Var{Rel: 0, Attr: 0, Typ: tuple.Int4, Alias: "u", Column: "a"}
	one  = &query.Const{Typ: tuple.Int4, Value: int32(1)}
	five = &query.Const{Typ: tuple.Int4, Value: int32(5)}
	aGt1 = &query.OpExpr{Op: ast.Gt, Typ: tuple.Bool, Left: va, Right: one}
	aEq1 = &query.OpExpr{Op: ast.Eq, Typ: tuple.Bool, Left: va, Right: one}
)

func TestRange(t *testing.T) {
	scan := &SeqScan{Rel: t0}
	for _, n := range []Node{scan, &Filter{Input: scan}, &Sort{Input: scan}, &Limit{Input: scan}, &IndexScan{Rel: t0, Index: iInfo}} {
		if r := n.Range(); len(r) != 1 || r[0] != t0 {
			t.Errorf("%T.Range() = %v, want [t]", n, r)
		}
	}
	for _, n := range []Node{&Result{}, &Values{}, &Project{Input: scan}, &ModifyTable{Input: scan}} {
		if r := n.Range(); r != nil {
			t.Errorf("%T.Range() = %v, want nil", n, r)
		}
	}
}

func TestExplain(t *testing.T) {
	cases := []struct {
		name string
		plan Node
		want string
	}{
		{"result", &Project{Input: &Result{}, Targets: []query.Target{{Name: "?column?", Expr: one}}},
			"Result"},
		{"scan", &SeqScan{Rel: t0}, "Seq Scan on t"},
		{"alias", &SeqScan{Rel: tu}, "Seq Scan on t u"},
		{"filter", &Filter{Input: &SeqScan{Rel: t0}, Qual: aGt1}, `
Seq Scan on t
  Filter: (t.a > 1)`},
		{"project is invisible", &Project{Input: &Filter{Input: &SeqScan{Rel: t0}, Qual: aGt1},
			Targets: []query.Target{{Name: "a", Expr: va}}}, `
Seq Scan on t
  Filter: (t.a > 1)`},
		{"sort", &Sort{Input: &SeqScan{Rel: t0}, Keys: []query.SortKey{{Expr: vb}, {Expr: va, Desc: true}}}, `
Sort
  Sort Key: t.b, t.a DESC
  ->  Seq Scan on t`},
		{"full select", &Limit{Count: &query.Const{Typ: tuple.Int8, Value: int64(2)},
			Input: &Project{Targets: []query.Target{{Name: "a", Expr: va}},
				Input: &Sort{Keys: []query.SortKey{{Expr: va, Desc: true}},
					Input: &Filter{Qual: aGt1, Input: &SeqScan{Rel: t0}}}}}, `
Limit
  ->  Sort
        Sort Key: t.a DESC
        ->  Seq Scan on t
              Filter: (t.a > 1)`},
		{"one-time filter", &Project{Input: &Filter{Input: &Result{}, Qual: &query.Const{Typ: tuple.Bool, Value: false}},
			Targets: []query.Target{{Name: "?column?", Expr: one}}}, `
Result
  One-Time Filter: FALSE`},
		{"insert", &ModifyTable{Op: Insert, Rel: t0, Input: &Values{Rows: [][]query.Expr{{one, vb}}}}, `
Insert on t
  ->  Values Scan on "*VALUES*"`},
		{"update", &ModifyTable{Op: Update, Rel: t0, Set: []query.Assignment{{Attr: 0, Value: one}},
			Input: &Filter{Input: &SeqScan{Rel: t0}, Qual: aGt1}}, `
Update on t
  ->  Seq Scan on t
        Filter: (t.a > 1)`},
		{"delete", &ModifyTable{Op: Delete, Rel: t0, Input: &SeqScan{Rel: t0}}, `
Delete on t
  ->  Seq Scan on t`},
		{"index scan", &IndexScan{Rel: t0, Index: iInfo, Quals: []query.Expr{aEq1}}, `
Index Scan using i on t
  Index Cond: (t.a = 1)`},
		{"index scan, alias and two conds", &IndexScan{Rel: tu, Index: iInfo, Quals: []query.Expr{
			&query.OpExpr{Op: ast.Gt, Typ: tuple.Bool, Left: ua, Right: one},
			&query.OpExpr{Op: ast.Le, Typ: tuple.Bool, Left: ua, Right: five},
		}}, `
Index Scan using i on t u
  Index Cond: ((u.a > 1) AND (u.a <= 5))`},
		{"index scan without cond", &IndexScan{Rel: t0, Index: iInfo}, "Index Scan using i on t"},
		{"index scan with filter", &Filter{Input: &IndexScan{Rel: t0, Index: iInfo, Quals: []query.Expr{aGt1}},
			Qual: &query.NullTest{X: vb}}, `
Index Scan using i on t
  Index Cond: (t.a > 1)
  Filter: (t.b IS NULL)`},
	}
	for _, c := range cases {
		got := strings.Join(Explain(c.plan), "\n")
		if got != strings.TrimPrefix(c.want, "\n") {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, strings.TrimPrefix(c.want, "\n"))
		}
	}
}

func TestModifyOpString(t *testing.T) {
	for op, want := range map[ModifyOp]string{Insert: "Insert", Update: "Update", Delete: "Delete"} {
		if op.String() != want {
			t.Errorf("%d.String() = %q, want %q", op, op.String(), want)
		}
	}
}

// Chapter 14: estimates.

func TestEstimateString(t *testing.T) {
	cases := []struct {
		est  Estimate
		want string
	}{
		{Estimate{}, "cost=0.00..0.00 rows=0 width=0"},
		{Estimate{StartupCost: 0.15, TotalCost: 8.17, Rows: 1, Width: 36}, "cost=0.15..8.17 rows=1 width=36"},
		{Estimate{TotalCost: 22.0125, Rows: 1201, Width: 36}, "cost=0.00..22.01 rows=1201 width=36"},
		{Estimate{StartupCost: 14304.815, TotalCost: 14554.8, Rows: 100000, Width: 4}, "cost=14304.82..14554.80 rows=100000 width=4"},
	}
	for _, c := range cases {
		if got := c.est.String(); got != c.want {
			t.Errorf("%+v.String() = %q, want %q", c.est, got, c.want)
		}
	}
}

func TestExplainCosts(t *testing.T) {
	scan := &SeqScan{Rel: t0, Est: Estimate{TotalCost: 22.01, Rows: 1201, Width: 36}}
	// A Filter and a Project lend their estimates to the scan's line.
	filter := &Filter{Input: scan, Qual: aGt1, Est: Estimate{TotalCost: 25.01, Rows: 400, Width: 36}}
	proj := &Project{Input: filter, Targets: []query.Target{{Name: "a", Expr: va}}, Est: Estimate{TotalCost: 25.01, Rows: 400, Width: 4}}
	sort := &Sort{Input: proj, Keys: []query.SortKey{{Expr: vb}}, Est: Estimate{StartupCost: 42.3, TotalCost: 43.3, Rows: 400, Width: 4}}
	lim := &Limit{Input: sort, Count: &query.Const{Typ: tuple.Int8, Value: int64(2)}, Est: Estimate{StartupCost: 42.3, TotalCost: 42.31, Rows: 2, Width: 4}}
	cases := []struct {
		name string
		plan Node
		want string
	}{
		{"scan", scan, "Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)"},
		{"filter", filter, `
Seq Scan on t  (cost=0.00..25.01 rows=400 width=36)
  Filter: (t.a > 1)`},
		{"project", proj, `
Seq Scan on t  (cost=0.00..25.01 rows=400 width=4)
  Filter: (t.a > 1)`},
		{"tree", lim, `
Limit  (cost=42.30..42.31 rows=2 width=4)
  ->  Sort  (cost=42.30..43.30 rows=400 width=4)
        Sort Key: t.b
        ->  Seq Scan on t  (cost=0.00..25.01 rows=400 width=4)
              Filter: (t.a > 1)`},
		{"index scan", &IndexScan{Rel: tu, Index: iInfo, Quals: []query.Expr{aEq1}, Est: Estimate{StartupCost: 0.15, TotalCost: 8.17, Rows: 1, Width: 36}},
			`
Index Scan using i on t u  (cost=0.15..8.17 rows=1 width=36)
  Index Cond: (t.a = 1)`},
		{"result", &Project{Input: &Result{Est: Estimate{TotalCost: 0.01, Rows: 1, Width: 4}},
			Targets: []query.Target{{Name: "?column?", Expr: one}}, Est: Estimate{TotalCost: 0.01, Rows: 1, Width: 4}},
			"Result  (cost=0.00..0.01 rows=1 width=4)"},
		{"insert", &ModifyTable{Op: Insert, Rel: t0, Input: &Values{Rows: [][]query.Expr{{one, vb}}, Est: Estimate{TotalCost: 0.0125, Rows: 1, Width: 36}}},
			`
Insert on t  (cost=0.00..0.00 rows=0 width=0)
  ->  Values Scan on "*VALUES*"  (cost=0.00..0.01 rows=1 width=36)`},
	}
	for _, c := range cases {
		got := strings.Join(ExplainCosts(c.plan), "\n")
		if got != strings.TrimPrefix(c.want, "\n") {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, strings.TrimPrefix(c.want, "\n"))
		}
	}
	// Explain stays cost-free, and a hand-built tree has zero estimates.
	if got := strings.Join(Explain(lim), "\n"); strings.Contains(got, "cost=") {
		t.Errorf("Explain prints costs:\n%s", got)
	}
	if (&SeqScan{Rel: t0}).Estimate() != (Estimate{}) {
		t.Error("a fresh node has a non-zero estimate")
	}
}

// Chapter 15: joins.

var (
	uInfo = &catalog.RelationInfo{OID: 16386, Name: "u", Kind: catalog.RelKindTable, Desc: tuple.NewDesc(
		tuple.Attr{Name: "x", Type: tuple.Int4},
		tuple.Attr{Name: "y", Type: tuple.Int8},
	)}
	u1   = &query.RangeEntry{Index: 1, Alias: "u", Rel: uInfo}
	t2   = &query.RangeEntry{Index: 2, Alias: "t2", Rel: tInfo}
	ux   = &query.Var{Rel: 1, Attr: 0, Typ: tuple.Int4, Alias: "u", Column: "x"}
	aEqX = &query.OpExpr{Op: ast.Eq, Typ: tuple.Bool, Left: va, Right: ux}
	aLtX = &query.OpExpr{Op: ast.Lt, Typ: tuple.Bool, Left: va, Right: ux}
)

func TestJoinRange(t *testing.T) {
	ts, us := &SeqScan{Rel: t0}, &SeqScan{Rel: u1}
	for _, n := range []Node{
		&NestLoop{Outer: ts, Inner: &Materialize{Input: us}},
		&HashJoin{Outer: ts, Inner: &Hash{Input: us}},
	} {
		if r := n.Range(); len(r) != 2 || r[0] != t0 || r[1] != u1 {
			t.Errorf("%T.Range() = %v, want [t u]", n, r)
		}
	}
	// The order is the join's, not the range table's.
	if r := (&NestLoop{Outer: us, Inner: ts}).Range(); len(r) != 2 || r[0] != u1 || r[1] != t0 {
		t.Errorf("NestLoop(u, t).Range() = %v, want [u t]", r)
	}
	for _, n := range []Node{&Hash{Input: us}, &Materialize{Input: us}} {
		if r := n.Range(); len(r) != 1 || r[0] != u1 {
			t.Errorf("%T.Range() = %v, want [u]", n, r)
		}
	}
	// A join's range is a fresh slice: the children's stay as they were.
	nl := &NestLoop{Outer: &NestLoop{Outer: ts, Inner: us}, Inner: &SeqScan{Rel: t2}}
	_ = append(nl.Outer.Range(), t2)
	if r := nl.Range(); len(r) != 3 || r[2] != t2 || len(nl.Outer.Range()) != 2 {
		t.Errorf("nested Range() = %v", r)
	}
}

func TestExplainJoins(t *testing.T) {
	ts, us := &SeqScan{Rel: t0}, &SeqScan{Rel: u1}
	cases := []struct {
		name string
		plan Node
		want string
	}{
		{"nested loop", &NestLoop{Outer: ts, Inner: &Materialize{Input: us}}, `
Nested Loop
  ->  Seq Scan on t
  ->  Materialize
        ->  Seq Scan on u`},
		{"join filter", &NestLoop{Outer: ts, Inner: &Materialize{Input: us}, Qual: aLtX}, `
Nested Loop
  Join Filter: (t.a < u.x)
  ->  Seq Scan on t
  ->  Materialize
        ->  Seq Scan on u`},
		{"parameterised index scan", &NestLoop{Outer: us, Inner: &IndexScan{Rel: t0, Index: iInfo, Quals: []query.Expr{aEqX}}}, `
Nested Loop
  ->  Seq Scan on u
  ->  Index Scan using i on t
        Index Cond: (t.a = u.x)`},
		{"hash join", &HashJoin{Outer: ts, Inner: &Hash{Input: us}, HashQuals: []query.Expr{aEqX}}, `
Hash Join
  Hash Cond: (t.a = u.x)
  ->  Seq Scan on t
  ->  Hash
        ->  Seq Scan on u`},
		{"hash join, two conds and a filter", &HashJoin{Outer: ts, Inner: &Hash{Input: us},
			HashQuals: []query.Expr{aEqX, &query.OpExpr{Op: ast.Eq, Typ: tuple.Bool,
				Left: &query.OpExpr{Op: ast.Add, Typ: tuple.Int4, Left: va, Right: one}, Right: ux}},
			Qual: aLtX}, `
Hash Join
  Hash Cond: ((t.a = u.x) AND ((t.a + 1) = u.x))
  Join Filter: (t.a < u.x)
  ->  Seq Scan on t
  ->  Hash
        ->  Seq Scan on u`},
		{"three tables", &Project{Targets: []query.Target{{Name: "a", Expr: va}},
			Input: &NestLoop{Qual: aLtX,
				Outer: &HashJoin{Outer: ts, Inner: &Hash{Input: &Filter{Input: us, Qual: aGt1}}, HashQuals: []query.Expr{aEqX}},
				Inner: &Materialize{Input: &SeqScan{Rel: t2}}}}, `
Nested Loop
  Join Filter: (t.a < u.x)
  ->  Hash Join
        Hash Cond: (t.a = u.x)
        ->  Seq Scan on t
        ->  Hash
              ->  Seq Scan on u
                    Filter: (t.a > 1)
  ->  Materialize
        ->  Seq Scan on t t2`},
		{"sort over a join", &Sort{Keys: []query.SortKey{{Expr: ux}},
			Input: &HashJoin{Outer: ts, Inner: &Hash{Input: us}, HashQuals: []query.Expr{aEqX}}}, `
Sort
  Sort Key: u.x
  ->  Hash Join
        Hash Cond: (t.a = u.x)
        ->  Seq Scan on t
        ->  Hash
              ->  Seq Scan on u`},
	}
	for _, c := range cases {
		got := strings.Join(Explain(c.plan), "\n")
		if got != strings.TrimPrefix(c.want, "\n") {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, strings.TrimPrefix(c.want, "\n"))
		}
	}
	// With costs: a Hash node's startup cost is its total cost.
	hj := &HashJoin{HashQuals: []query.Expr{aEqX},
		Outer: &SeqScan{Rel: t0, Est: Estimate{TotalCost: 22.01, Rows: 1201, Width: 36}},
		Inner: &Hash{Input: &SeqScan{Rel: u1, Est: Estimate{TotalCost: 20.75, Rows: 1075, Width: 44}},
			Est: Estimate{StartupCost: 20.75, TotalCost: 20.75, Rows: 1075, Width: 44}},
		Est: Estimate{StartupCost: 34.19, TotalCost: 285.88, Rows: 6455, Width: 80}}
	want := strings.TrimPrefix(`
Hash Join  (cost=34.19..285.88 rows=6455 width=80)
  Hash Cond: (t.a = u.x)
  ->  Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)
  ->  Hash  (cost=20.75..20.75 rows=1075 width=44)
        ->  Seq Scan on u  (cost=0.00..20.75 rows=1075 width=44)`, "\n")
	if got := strings.Join(ExplainCosts(hj), "\n"); got != want {
		t.Errorf("hash join with costs:\n%s\nwant\n%s", got, want)
	}
}
