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

var (
	t0   = &query.RangeEntry{Alias: "t", Rel: tInfo}
	tu   = &query.RangeEntry{Alias: "u", Rel: tInfo}
	va   = &query.Var{Rel: 0, Attr: 0, Typ: tuple.Int4, Alias: "t", Column: "a"}
	vb   = &query.Var{Rel: 0, Attr: 1, Typ: tuple.Text, Alias: "t", Column: "b"}
	one  = &query.Const{Typ: tuple.Int4, Value: int32(1)}
	aGt1 = &query.OpExpr{Op: ast.Gt, Typ: tuple.Bool, Left: va, Right: one}
)

func TestRange(t *testing.T) {
	scan := &SeqScan{Rel: t0}
	for _, n := range []Node{scan, &Filter{Input: scan}, &Sort{Input: scan}, &Limit{Input: scan}} {
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
