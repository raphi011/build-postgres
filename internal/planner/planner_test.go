package planner

import (
	"errors"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/plan"
	"github.com/raphi011/build-postgres/internal/sql/analyzer"
	"github.com/raphi011/build-postgres/internal/sql/parser"
	"github.com/raphi011/build-postgres/internal/sql/query"
	"github.com/raphi011/build-postgres/internal/tuple"
)

type fakeCatalog map[string]*catalog.RelationInfo

func (f fakeCatalog) Lookup(name string) (*catalog.RelationInfo, error) {
	if info, ok := f[name]; ok {
		return info, nil
	}
	return nil, catalog.ErrNotFound
}

var cat = fakeCatalog{
	"t": {OID: 16384, Name: "t", Kind: catalog.RelKindTable, Desc: tuple.NewDesc(
		tuple.Attr{Name: "a", Type: tuple.Int4, NotNull: true},
		tuple.Attr{Name: "b", Type: tuple.Text},
	)},
}

func analyze(t *testing.T, src string) query.Stmt {
	t.Helper()
	stmts, err := parser.Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	q, err := analyzer.Analyze(stmts[0], cat)
	if err != nil {
		t.Fatalf("Analyze(%q): %v", src, err)
	}
	return q
}

func TestPlanGolden(t *testing.T) {
	cases := []struct{ src, want string }{
		{"select 1", "Result"},
		{"select 1 where false", `
Result
  One-Time Filter: FALSE`},
		{"select * from t", "Seq Scan on t"},
		{"select a from t as x where x.b = 'k'", `
Seq Scan on t x
  Filter: (x.b = 'k')`},
		{"select a from t where a > 1 and b is not null order by b desc, a limit 10", `
Limit
  ->  Sort
        Sort Key: t.b DESC, t.a
        ->  Seq Scan on t
              Filter: ((t.a > 1) AND (t.b IS NOT NULL))`},
		{"select a + 1 as x from t order by x", `
Sort
  Sort Key: (t.a + 1)
  ->  Seq Scan on t`},
		{"select a from t limit 1", `
Limit
  ->  Seq Scan on t`},
		{"insert into t values (1, 'x'), (2, 'y')", `
Insert on t
  ->  Values Scan on "*VALUES*"`},
		{"update t set b = 'z' where a = 1", `
Update on t
  ->  Seq Scan on t
        Filter: (t.a = 1)`},
		{"update t set b = 'z'", `
Update on t
  ->  Seq Scan on t`},
		{"delete from t where b is null", `
Delete on t
  ->  Seq Scan on t
        Filter: (t.b IS NULL)`},
		{"delete from t", `
Delete on t
  ->  Seq Scan on t`},
	}
	for _, c := range cases {
		p, err := Plan(analyze(t, c.src))
		if err != nil {
			t.Errorf("Plan(%q): %v", c.src, err)
			continue
		}
		got := strings.Join(plan.Explain(p), "\n")
		if want := strings.TrimPrefix(c.want, "\n"); got != want {
			t.Errorf("Plan(%q) =\n%s\nwant\n%s", c.src, got, want)
		}
	}
}

// TestPlanShape checks the node order Explain hides: Sort sits below
// Project because sort keys are over the range table, and Limit above it.
func TestPlanShape(t *testing.T) {
	p, err := Plan(analyze(t, "select a from t where a > 1 order by b limit 10"))
	if err != nil {
		t.Fatal(err)
	}
	limit, ok := p.(*plan.Limit)
	if !ok {
		t.Fatalf("root is %T, want *plan.Limit", p)
	}
	proj, ok := limit.Input.(*plan.Project)
	if !ok {
		t.Fatalf("under Limit is %T, want *plan.Project", limit.Input)
	}
	if len(proj.Targets) != 1 || proj.Targets[0].Name != "a" {
		t.Errorf("targets = %v", proj.Targets)
	}
	sort, ok := proj.Input.(*plan.Sort)
	if !ok {
		t.Fatalf("under Project is %T, want *plan.Sort", proj.Input)
	}
	filter, ok := sort.Input.(*plan.Filter)
	if !ok {
		t.Fatalf("under Sort is %T, want *plan.Filter", sort.Input)
	}
	if _, ok := filter.Input.(*plan.SeqScan); !ok {
		t.Fatalf("under Filter is %T, want *plan.SeqScan", filter.Input)
	}

	// No optional clauses, no optional nodes.
	p, _ = Plan(analyze(t, "select a from t"))
	proj, ok = p.(*plan.Project)
	if !ok {
		t.Fatalf("root is %T, want *plan.Project", p)
	}
	if _, ok := proj.Input.(*plan.SeqScan); !ok {
		t.Fatalf("under Project is %T, want *plan.SeqScan", proj.Input)
	}

	p, _ = Plan(analyze(t, "update t set a = 2, b = 'x' where a = 1"))
	mt, ok := p.(*plan.ModifyTable)
	if !ok || mt.Op != plan.Update || mt.Rel.Rel.Name != "t" || len(mt.Set) != 2 {
		t.Fatalf("update plan = %#v", p)
	}
	if mt.Set[0].Attr != 0 || mt.Set[1].Attr != 1 {
		t.Errorf("assignments = %v", mt.Set)
	}
	p, _ = Plan(analyze(t, "insert into t (a) values (1)"))
	mt, ok = p.(*plan.ModifyTable)
	if !ok || mt.Op != plan.Insert {
		t.Fatalf("insert plan = %#v", p)
	}
	vals, ok := mt.Input.(*plan.Values)
	if !ok || len(vals.Rows) != 1 || len(vals.Rows[0]) != 2 {
		t.Fatalf("insert input = %#v", mt.Input)
	}
}

func TestPlanErrors(t *testing.T) {
	cases := []struct {
		src string
		err error
	}{
		{"select * from t, t as u", ErrJoin},
		{"select 1 from t join t as u on t.a = u.a", ErrJoin},
		{"create table x (a int4)", ErrUtility},
		{"drop table t", ErrUtility},
		{"begin", ErrUtility},
		{"commit", ErrUtility},
		{"rollback", ErrUtility},
		{"explain select 1", ErrUtility},
	}
	for _, c := range cases {
		_, err := Plan(analyze(t, c.src))
		if !errors.Is(err, c.err) {
			t.Errorf("Plan(%q) = %v, want %v", c.src, err, c.err)
		}
	}
}
