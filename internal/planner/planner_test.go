package planner

import (
	"errors"
	"fmt"
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

// indexed builds a table (a int4, b text, c int8) with the given
// statistics and an index on a and a unique index on c, each with its own
// statistics.
func indexed(name string, oid tuple.OID, pages int32, tuples int64, indexPages int32) *catalog.RelationInfo {
	info := &catalog.RelationInfo{OID: oid, Name: name, Kind: catalog.RelKindTable, Pages: pages, Tuples: tuples,
		Desc: tuple.NewDesc(
			tuple.Attr{Name: "a", Type: tuple.Int4, NotNull: true},
			tuple.Attr{Name: "b", Type: tuple.Text},
			tuple.Attr{Name: "c", Type: tuple.Int8},
		)}
	var indexTuples int64
	if indexPages > 0 {
		indexTuples = tuples
	}
	info.Indexes = []*catalog.IndexInfo{
		{OID: oid + 1, Name: name + "_a", Rel: oid, Attr: 0, Pages: indexPages, Tuples: indexTuples},
		{OID: oid + 2, Name: name + "_c_key", Rel: oid, Attr: 2, Unique: true, Pages: indexPages, Tuples: indexTuples},
	}
	return info
}

// t has no index and no statistics; u has indexes and no statistics;
// small and big have indexes and statistics.
var cat = fakeCatalog{
	"t": {OID: 16384, Name: "t", Kind: catalog.RelKindTable, Desc: tuple.NewDesc(
		tuple.Attr{Name: "a", Type: tuple.Int4, NotNull: true},
		tuple.Attr{Name: "b", Type: tuple.Text},
	)},
	"u":     indexed("u", 16400, 0, 0, 0),
	"small": indexed("small", 16410, 1, 100, 2),
	"big":   indexed("big", 16420, 5000, 100000, 300),
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
		{"create table x (a int4)", ErrUtility},
		{"drop table t", ErrUtility},
		{"analyze t", ErrUtility},
		{"create index i on t (a)", ErrUtility},
		{"drop index i", ErrUtility},
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

// TestTooManyRelations checks that a FROM list past MaxJoinRelations is
// refused rather than planned, the join search being exponential.
func TestTooManyRelations(t *testing.T) {
	from := func(n int) string {
		names := make([]string, n)
		for i := range names {
			names[i] = fmt.Sprintf("t as t%d", i)
		}
		return "select 1 from " + strings.Join(names, ", ")
	}
	if _, err := Plan(analyze(t, from(3))); err != nil {
		t.Fatalf("Plan of 3 relations: %v", err)
	}
	if _, err := Plan(analyze(t, from(MaxJoinRelations+1))); !errors.Is(err, ErrTooManyRelations) {
		t.Errorf("Plan of %d relations = %v, want ErrTooManyRelations", MaxJoinRelations+1, err)
	}
}

// Chapter 14: costs and the scan choice.

// TestEstimates pins the cost model: the numbers follow the formulas in
// the chapter 14 README, with PostgreSQL's default parameters.
func TestEstimates(t *testing.T) {
	cases := []struct{ src, want string }{
		// A never-analysed table is taken to have 10 pages of rows as wide
		// as its columns: t is 36 bytes wide, 1201 rows.
		{"select * from t", "Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)"},
		{"select a from t where a > 1", `
Seq Scan on t  (cost=0.00..25.01 rows=400 width=4)
  Filter: (t.a > 1)`},
		{"select 1", "Result  (cost=0.00..0.01 rows=1 width=4)"},
		{"select 1, 'x', true where false", `
Result  (cost=0.00..0.01 rows=1 width=37)
  One-Time Filter: FALSE`},
		{"select a from t where a > 1 order by b limit 2", `
Limit  (cost=42.30..42.31 rows=2 width=4)
  ->  Sort  (cost=42.30..43.30 rows=400 width=4)
        Sort Key: t.b
        ->  Seq Scan on t  (cost=0.00..25.01 rows=400 width=36)
              Filter: (t.a > 1)`},
		// A LIMIT that is not a constant is taken as a tenth of the rows.
		{"select a from t limit 1 + 1", `
Limit  (cost=0.00..2.20 rows=120 width=4)
  ->  Seq Scan on t  (cost=0.00..22.01 rows=1201 width=4)`},
		{"insert into t values (1, 'x'), (2, 'y')", `
Insert on t  (cost=0.00..0.03 rows=0 width=0)
  ->  Values Scan on "*VALUES*"  (cost=0.00..0.03 rows=2 width=36)`},
		{"delete from t where b is null", `
Delete on t  (cost=0.00..22.01 rows=0 width=0)
  ->  Seq Scan on t  (cost=0.00..22.01 rows=6 width=36)
        Filter: (t.b IS NULL)`},
		// Selectivities: 0.005 for =, 1/3 for one inequality, 0.005 for a
		// range, 0.5 for a boolean column, OR and NOT as in PostgreSQL.
		{"select * from t where a = 1 and b <> 'x'", `
Seq Scan on t  (cost=0.00..28.02 rows=6 width=36)
  Filter: ((t.a = 1) AND (t.b <> 'x'))`},
		{"select * from t where a > 1 and a < 9", `
Seq Scan on t  (cost=0.00..28.02 rows=6 width=36)
  Filter: ((t.a > 1) AND (t.a < 9))`},
		{"select * from t where a > 1 and a > 2", `
Seq Scan on t  (cost=0.00..28.02 rows=400 width=36)
  Filter: ((t.a > 1) AND (t.a > 2))`},
		{"select * from t where a = 1 or a = 2", `
Seq Scan on t  (cost=0.00..28.02 rows=12 width=36)
  Filter: ((t.a = 1) OR (t.a = 2))`},
		{"select * from t where not a = 1", `
Seq Scan on t  (cost=0.00..25.01 rows=1195 width=36)
  Filter: (NOT (t.a = 1))`},
		{"select * from t where b is not null", `
Seq Scan on t  (cost=0.00..22.01 rows=1195 width=36)
  Filter: (t.b IS NOT NULL)`},
		{"select * from t where a + 1 = 2", `
Seq Scan on t  (cost=0.00..28.02 rows=6 width=36)
  Filter: ((t.a + 1) = 2)`},
	}
	for _, c := range cases {
		p, err := Plan(analyze(t, c.src))
		if err != nil {
			t.Errorf("Plan(%q): %v", c.src, err)
			continue
		}
		got := strings.Join(plan.ExplainCosts(p), "\n")
		if want := strings.TrimPrefix(c.want, "\n"); got != want {
			t.Errorf("Plan(%q) =\n%s\nwant\n%s", c.src, got, want)
		}
	}
}

// TestScanChoice checks which scan the cost model picks, and the plan
// around it, for tables of different sizes.
func TestScanChoice(t *testing.T) {
	cases := []struct{ src, want string }{
		// Never analysed: 10 pages, so an equality on an indexed column
		// uses the index and a single inequality does not.
		{"select * from u where a = 5", `
Index Scan using u_a on u  (cost=0.15..20.24 rows=5 width=44)
  Index Cond: (u.a = 5)`},
		{"select * from u where a > 5", `
Seq Scan on u  (cost=0.00..23.44 rows=358 width=44)
  Filter: (u.a > 5)`},
		{"select * from u where a > 5 and a < 10", `
Index Scan using u_a on u  (cost=0.15..20.26 rows=5 width=44)
  Index Cond: ((u.a > 5) AND (u.a < 10))`},
		// The column may be on either side; the rest becomes a Filter.
		{"select * from u where 5 = a and b = 'x'", `
Index Scan using u_a on u  (cost=0.15..20.26 rows=1 width=44)
  Index Cond: (u.a = 5)
  Filter: (u.b = 'x')`},
		{"select * from u where b = 'x'", `
Seq Scan on u  (cost=0.00..23.44 rows=5 width=44)
  Filter: (u.b = 'x')`},
		// Two usable indexes: the cheaper wins, the first on a tie.
		{"select * from u where a = 1 and c = 2", `
Index Scan using u_a on u  (cost=0.15..20.26 rows=1 width=44)
  Index Cond: (u.a = 1)
  Filter: (u.c = 2::int8)`},
		// An index in ORDER BY order replaces the Sort when that is
		// cheaper, which a LIMIT makes it; a descending order cannot use
		// the index (no backward scans).
		{"select a from u order by a", "Index Scan using u_a on u  (cost=0.15..60.28 rows=1075 width=4)"},
		{"select a from u order by a limit 1", `
Limit  (cost=0.15..0.21 rows=1 width=4)
  ->  Index Scan using u_a on u  (cost=0.15..60.28 rows=1075 width=4)`},
		{"select a from u order by a desc limit 1", `
Limit  (cost=74.88..74.88 rows=1 width=4)
  ->  Sort  (cost=74.88..77.56 rows=1075 width=4)
        Sort Key: u.a DESC
        ->  Seq Scan on u  (cost=0.00..20.75 rows=1075 width=44)`},
		{"select a from u where a > 5 order by a limit 1", `
Limit  (cost=0.15..0.29 rows=1 width=4)
  ->  Index Scan using u_a on u  (cost=0.15..50.42 rows=358 width=4)
        Index Cond: (u.a > 5)`},
		// UPDATE and DELETE scan the same way.
		{"update u set b = 'y' where a = 5", `
Update on u  (cost=0.15..20.24 rows=0 width=0)
  ->  Index Scan using u_a on u  (cost=0.15..20.24 rows=5 width=44)
        Index Cond: (u.a = 5)`},
		{"delete from u where a > 5", `
Delete on u  (cost=0.00..23.44 rows=0 width=0)
  ->  Seq Scan on u  (cost=0.00..23.44 rows=358 width=44)
        Filter: (u.a > 5)`},
		// A small analysed table is read sequentially, whatever the qual;
		// the index only pays off for ORDER BY ... LIMIT.
		{"select * from small where a = 5", `
Seq Scan on small  (cost=0.00..2.25 rows=1 width=44)
  Filter: (small.a = 5)`},
		{"select a from small order by a", `
Sort  (cost=5.32..5.57 rows=100 width=4)
  Sort Key: small.a
  ->  Seq Scan on small  (cost=0.00..2.00 rows=100 width=44)`},
		{"select a from small order by a limit 1", `
Limit  (cost=0.14..0.28 rows=1 width=4)
  ->  Index Scan using small_a on small  (cost=0.14..13.64 rows=100 width=4)`},
		// A big analysed table: the index wins for = and for a range,
		// loses for a single inequality, and sorting beats a full index
		// scan without a LIMIT.
		{"select * from big where a = 5", `
Index Scan using big_a on big  (cost=0.17..1924.92 rows=500 width=44)
  Index Cond: (big.a = 5)`},
		{"select * from big where c > 5 and c <= 10", `
Index Scan using big_c_key on big  (cost=0.17..1926.17 rows=500 width=44)
  Index Cond: ((big.c > 5::int8) AND (big.c <= 10::int8))`},
		{"select * from big where a > 5", `
Seq Scan on big  (cost=0.00..6250.00 rows=33333 width=44)
  Filter: (big.a > 5)`},
		{"select a from big order by a", `
Sort  (cost=14304.82..14554.82 rows=100000 width=4)
  Sort Key: big.a
  ->  Seq Scan on big  (cost=0.00..6000.00 rows=100000 width=44)`},
		{"select a from big order by a limit 1", `
Limit  (cost=0.17..0.39 rows=1 width=4)
  ->  Index Scan using big_a on big  (cost=0.17..22700.17 rows=100000 width=4)`},
	}
	for _, c := range cases {
		p, err := Plan(analyze(t, c.src))
		if err != nil {
			t.Errorf("Plan(%q): %v", c.src, err)
			continue
		}
		got := strings.Join(plan.ExplainCosts(p), "\n")
		if want := strings.TrimPrefix(c.want, "\n"); got != want {
			t.Errorf("Plan(%q) =\n%s\nwant\n%s", c.src, got, want)
		}
	}
}

// TestIndexScanShape checks the nodes the cost output hides: the index
// scan's quals are rewritten with the column on the left, the rest
// becomes a Filter, and Project sits above them.
func TestIndexScanShape(t *testing.T) {
	p, err := Plan(analyze(t, "select b from u where 5 = a and b = 'x' order by a limit 3"))
	if err != nil {
		t.Fatal(err)
	}
	limit := p.(*plan.Limit)
	proj := limit.Input.(*plan.Project)
	filter, ok := proj.Input.(*plan.Filter)
	if !ok {
		t.Fatalf("under Project is %T, want *plan.Filter (no Sort: the index provides the order)", proj.Input)
	}
	scan, ok := filter.Input.(*plan.IndexScan)
	if !ok {
		t.Fatalf("under Filter is %T, want *plan.IndexScan", filter.Input)
	}
	if scan.Index.Name != "u_a" || len(scan.Quals) != 1 || scan.Quals[0].String() != "(u.a = 5)" {
		t.Errorf("index scan = %s using %s", scan.Quals, scan.Index.Name)
	}
	if filter.Qual.String() != "(u.b = 'x')" {
		t.Errorf("filter = %s", filter.Qual)
	}
	// Estimates are set on every node, Filter and Project included.
	for _, n := range []plan.Node{limit, proj, filter, scan} {
		if n.Estimate().TotalCost <= 0 || n.Estimate().Rows < 1 {
			t.Errorf("%T has no estimate: %+v", n, n.Estimate())
		}
	}
	// A qual on another column of the same table is not an index qual.
	p, _ = Plan(analyze(t, "select * from u where a = c"))
	if _, ok := p.(*plan.Project).Input.(*plan.Filter).Input.(*plan.SeqScan); !ok {
		t.Errorf("a = c planned as %T", p.(*plan.Project).Input)
	}
}

// Chapter 15: joins.

// TestJoinGolden pins the join method, the join order, and where each
// qual lands, without costs.
func TestJoinGolden(t *testing.T) {
	cases := []struct{ src, want string }{
		// A cross join is a nested loop over a materialised inner side.
		{"select * from t, u", `
Nested Loop
  ->  Seq Scan on t
  ->  Materialize
        ->  Seq Scan on u`},
		// An equality between the two sides makes a hash join; the hash
		// cond has the outer side on the left whichever way it was written.
		{"select * from t join u on t.a = u.a", `
Hash Join
  Hash Cond: (t.a = u.a)
  ->  Seq Scan on t
  ->  Hash
        ->  Seq Scan on u`},
		{"select * from t join u on u.a = t.a", `
Hash Join
  Hash Cond: (t.a = u.a)
  ->  Seq Scan on t
  ->  Hash
        ->  Seq Scan on u`},
		{"select * from t join u on t.a = u.c", `
Hash Join
  Hash Cond: (t.a::int8 = u.c)
  ->  Seq Scan on t
  ->  Hash
        ->  Seq Scan on u`},
		{"select * from t join u on t.a = u.a and t.b = u.b", `
Hash Join
  Hash Cond: ((u.a = t.a) AND (u.b = t.b))
  ->  Seq Scan on u
  ->  Hash
        ->  Seq Scan on t`},
		// Anything else stays a nested loop with a Join Filter.
		{"select * from t join u on t.a < u.a", `
Nested Loop
  Join Filter: (t.a < u.a)
  ->  Seq Scan on t
  ->  Materialize
        ->  Seq Scan on u`},
		{"select * from t, u where t.a = u.a or t.b = u.b", `
Nested Loop
  Join Filter: ((t.a = u.a) OR (t.b = u.b))
  ->  Seq Scan on t
  ->  Materialize
        ->  Seq Scan on u`},
		// A qual on one relation goes below the join; a mixed one stays
		// at the join.
		{"select * from t join u on t.a = u.a where t.b = 'x' and u.c > 1", `
Hash Join
  Hash Cond: (u.a = t.a)
  ->  Seq Scan on u
        Filter: (u.c > 1::int8)
  ->  Hash
        ->  Seq Scan on t
              Filter: (t.b = 'x')`},
		{"select * from t, u where t.a = u.a and u.c = t.a", `
Hash Join
  Hash Cond: ((t.a = u.a) AND (t.a::int8 = u.c))
  ->  Seq Scan on t
  ->  Hash
        ->  Seq Scan on u`},
		{"select * from t, u where true", `
Nested Loop
  ->  Seq Scan on t
        Filter: TRUE
  ->  Materialize
        ->  Seq Scan on u`},
		// A big indexed inner: a nested loop probing the index once per
		// outer row, with the join qual as the index condition.
		{"select * from small join big on small.a = big.c", `
Nested Loop
  ->  Seq Scan on small
  ->  Index Scan using big_c_key on big
        Index Cond: (big.c = small.a::int8)`},
		{"select * from small, big where big.c = small.a and big.b = 'x'", `
Nested Loop
  ->  Seq Scan on small
  ->  Index Scan using big_c_key on big
        Index Cond: (big.c = small.a::int8)
        Filter: (big.b = 'x')`},
		// A non-unique index on big does not pay: the hash join wins,
		// hashing the small side.
		{"select * from small join big on small.a = big.a", `
Hash Join
  Hash Cond: (big.a = small.a)
  ->  Seq Scan on big
  ->  Hash
        ->  Seq Scan on small`},
		// The outer side may be an index scan of its own.
		{"select * from big, big as b2 where big.a = b2.c and big.c = 5", `
Nested Loop
  ->  Index Scan using big_c_key on big
        Index Cond: (big.c = 5::int8)
  ->  Index Scan using big_c_key on big b2
        Index Cond: (b2.c = big.a::int8)`},
		// Three relations: a chain of parameterised scans, the qual over
		// the first and last at the top.
		{"select * from small, big, big as b2 where small.a = big.c and big.a = b2.c and b2.b = small.b", `
Nested Loop
  Join Filter: (b2.b = small.b)
  ->  Nested Loop
        ->  Seq Scan on small
        ->  Index Scan using big_c_key on big
              Index Cond: (big.c = small.a::int8)
  ->  Index Scan using big_c_key on big b2
        Index Cond: (b2.c = big.a::int8)`},
		// A qual over three relations waits for the join that has them all.
		{"select * from t, u, small where t.a + u.a = small.a", `
Hash Join
  Hash Cond: ((t.a + u.a) = small.a)
  ->  Nested Loop
        ->  Seq Scan on t
        ->  Materialize
              ->  Seq Scan on u
  ->  Hash
        ->  Seq Scan on small`},
		{"select * from t join u on t.a = u.a join big on u.c = big.c", `
Hash Join
  Hash Cond: (t.a = u.a)
  ->  Seq Scan on t
  ->  Hash
        ->  Hash Join
              Hash Cond: (big.c = u.c)
              ->  Seq Scan on big
              ->  Hash
                    ->  Seq Scan on u`},
		{"select * from t, u, small, big where t.a = u.a and u.c = small.c and small.a = big.c", `
Hash Join
  Hash Cond: (t.a = u.a)
  ->  Seq Scan on t
  ->  Hash
        ->  Nested Loop
              ->  Hash Join
                    Hash Cond: (u.c = small.c)
                    ->  Seq Scan on u
                    ->  Hash
                          ->  Seq Scan on small
              ->  Index Scan using big_c_key on big
                    Index Cond: (big.c = small.a::int8)`},
		// Self joins are joins like any other.
		{"select * from t, t as t2 where t.a = t2.a", `
Hash Join
  Hash Cond: (t.a = t2.a)
  ->  Seq Scan on t
  ->  Hash
        ->  Seq Scan on t t2`},
		// ORDER BY sorts above the join, unless a nested loop's outer side
		// delivers the order; a hash join delivers none.
		{"select * from t join u on t.a = u.a order by u.a", `
Sort
  Sort Key: u.a
  ->  Hash Join
        Hash Cond: (t.a = u.a)
        ->  Seq Scan on t
        ->  Hash
              ->  Seq Scan on u`},
		{"select * from small join big on small.a = big.c order by small.a limit 1", `
Limit
  ->  Nested Loop
        ->  Index Scan using small_a on small
        ->  Index Scan using big_c_key on big
              Index Cond: (big.c = small.a::int8)`},
		// A LIMIT favours the plan that starts early: a nested loop with
		// no setup over a hash join that must build its table first.
		{"select u.a from t, u where t.a = u.a limit 1", `
Limit
  ->  Nested Loop
        Join Filter: (t.a = u.a)
        ->  Seq Scan on t
        ->  Materialize
              ->  Seq Scan on u`},
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

// TestJoinEstimates pins the join cost model: the numbers follow the
// formulas in the chapter 15 README.
func TestJoinEstimates(t *testing.T) {
	cases := []struct{ src, want string }{
		{"select * from t, u", `
Nested Loop  (cost=0.00..16183.89 rows=1291075 width=80)
  ->  Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)
  ->  Materialize  (cost=0.00..26.12 rows=1075 width=44)
        ->  Seq Scan on u  (cost=0.00..20.75 rows=1075 width=44)`},
		// Rows: 1201 * 1075 / 200 distinct values.
		{"select * from t join u on t.a = u.a", `
Hash Join  (cost=34.19..285.88 rows=6455 width=80)
  Hash Cond: (t.a = u.a)
  ->  Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)
  ->  Hash  (cost=20.75..20.75 rows=1075 width=44)
        ->  Seq Scan on u  (cost=0.00..20.75 rows=1075 width=44)`},
		// u.c is unique: one row per t row.
		{"select * from t join u on t.a = u.c", `
Hash Join  (cost=34.19..74.21 rows=1201 width=80)
  Hash Cond: (t.a::int8 = u.c)
  ->  Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)
  ->  Hash  (cost=20.75..20.75 rows=1075 width=44)
        ->  Seq Scan on u  (cost=0.00..20.75 rows=1075 width=44)`},
		{"select * from t join u on t.a < u.a", `
Nested Loop  (cost=0.00..19411.57 rows=430358 width=80)
  Join Filter: (t.a < u.a)
  ->  Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)
  ->  Materialize  (cost=0.00..26.12 rows=1075 width=44)
        ->  Seq Scan on u  (cost=0.00..20.75 rows=1075 width=44)`},
		{"select * from t join u on t.a = u.a where t.b = 'x' and u.c > 1", `
Hash Join  (cost=25.09..49.98 rows=11 width=80)
  Hash Cond: (u.a = t.a)
  ->  Seq Scan on u  (cost=0.00..23.44 rows=358 width=44)
        Filter: (u.c > 1::int8)
  ->  Hash  (cost=25.01..25.01 rows=6 width=36)
        ->  Seq Scan on t  (cost=0.00..25.01 rows=6 width=36)
              Filter: (t.b = 'x')`},
		// The parameterised scan: one row at 8.19 per outer row.
		{"select * from small join big on small.a = big.c", `
Nested Loop  (cost=0.17..821.75 rows=100 width=88)
  ->  Seq Scan on small  (cost=0.00..2.00 rows=100 width=44)
  ->  Index Scan using big_c_key on big  (cost=0.17..8.19 rows=1 width=44)
        Index Cond: (big.c = small.a::int8)`},
		{"select * from small join big on small.a = big.a", `
Hash Join  (cost=3.25..6878.25 rows=50000 width=88)
  Hash Cond: (big.a = small.a)
  ->  Seq Scan on big  (cost=0.00..6000.00 rows=100000 width=44)
  ->  Hash  (cost=2.00..2.00 rows=100 width=44)
        ->  Seq Scan on small  (cost=0.00..2.00 rows=100 width=44)`},
		{"select * from small join big on small.a = big.c order by small.a limit 1", `
Limit  (cost=0.31..8.64 rows=1 width=88)
  ->  Nested Loop  (cost=0.31..833.39 rows=100 width=88)
        ->  Index Scan using small_a on small  (cost=0.14..13.64 rows=100 width=44)
        ->  Index Scan using big_c_key on big  (cost=0.17..8.19 rows=1 width=44)
              Index Cond: (big.c = small.a::int8)`},
		{"select * from t, u, small where t.a + u.a = small.a", `
Hash Join  (cost=3.25..29097.89 rows=645538 width=124)
  Hash Cond: ((t.a + u.a) = small.a)
  ->  Nested Loop  (cost=0.00..16183.89 rows=1291075 width=80)
        ->  Seq Scan on t  (cost=0.00..22.01 rows=1201 width=36)
        ->  Materialize  (cost=0.00..26.12 rows=1075 width=44)
              ->  Seq Scan on u  (cost=0.00..20.75 rows=1075 width=44)
  ->  Hash  (cost=2.00..2.00 rows=100 width=44)
        ->  Seq Scan on small  (cost=0.00..2.00 rows=100 width=44)`},
	}
	for _, c := range cases {
		p, err := Plan(analyze(t, c.src))
		if err != nil {
			t.Errorf("Plan(%q): %v", c.src, err)
			continue
		}
		got := strings.Join(plan.ExplainCosts(p), "\n")
		if want := strings.TrimPrefix(c.want, "\n"); got != want {
			t.Errorf("Plan(%q) =\n%s\nwant\n%s", c.src, got, want)
		}
	}
}

// TestJoinShape checks what the EXPLAIN output hides: the node types,
// the join's range order, the parameterised scan's quals, and that
// every node carries an estimate.
func TestJoinShape(t *testing.T) {
	p, err := Plan(analyze(t, "select big.b from small, big where big.c = small.a and big.b is not null order by small.a limit 3"))
	if err != nil {
		t.Fatal(err)
	}
	lim := p.(*plan.Limit)
	proj := lim.Input.(*plan.Project)
	nl, ok := proj.Input.(*plan.NestLoop)
	if !ok {
		t.Fatalf("under Project is %T, want *plan.NestLoop (no Sort: the outer index provides the order)", proj.Input)
	}
	if _, ok := nl.Outer.(*plan.IndexScan); !ok {
		t.Fatalf("outer is %T, want *plan.IndexScan", nl.Outer)
	}
	filter, ok := nl.Inner.(*plan.Filter)
	if !ok {
		t.Fatalf("inner is %T, want *plan.Filter", nl.Inner)
	}
	scan := filter.Input.(*plan.IndexScan)
	if scan.Index.Name != "big_c_key" || len(scan.Quals) != 1 || scan.Quals[0].String() != "(big.c = small.a::int8)" {
		t.Errorf("inner scan = %s using %s", scan.Quals, scan.Index.Name)
	}
	if v := scan.Quals[0].(*query.OpExpr).Right.(*query.Cast).X.(*query.Var); v.Rel != 0 {
		t.Errorf("parameter refers to range entry %d, want 0 (small)", v.Rel)
	}
	if nl.Qual != nil {
		t.Errorf("join filter = %s, want none: the index scan enforces the join qual", nl.Qual)
	}
	if r := nl.Range(); len(r) != 2 || r[0].Alias != "small" || r[1].Alias != "big" {
		t.Errorf("join range = %v", r)
	}
	for _, n := range []plan.Node{lim, proj, nl, nl.Outer, filter, scan} {
		if n.Estimate().TotalCost <= 0 || n.Estimate().Rows < 1 {
			t.Errorf("%T has no estimate: %+v", n, n.Estimate())
		}
	}

	// A hash join's inner is a Hash; its range is outer then inner, which
	// here is u then t: the row layout no longer follows the range table.
	p, _ = Plan(analyze(t, "select t.b from t join u on t.a = u.a where t.b = 'x'"))
	hj := p.(*plan.Project).Input.(*plan.HashJoin)
	hash, ok := hj.Inner.(*plan.Hash)
	if !ok {
		t.Fatalf("hash join inner is %T, want *plan.Hash", hj.Inner)
	}
	if f, ok := hash.Input.(*plan.Filter); !ok {
		t.Errorf("under Hash is %T, want *plan.Filter", hash.Input)
	} else if _, ok := f.Input.(*plan.SeqScan); !ok {
		t.Errorf("under Filter is %T, want *plan.SeqScan", f.Input)
	}
	if r := hj.Range(); len(r) != 2 || r[0].Alias != "u" || r[1].Alias != "t" || r[0].Index != 1 || r[1].Index != 0 {
		t.Errorf("hash join range = %v", r)
	}
	if h := hash.Estimate(); h.StartupCost != h.TotalCost || h.Rows != hash.Input.Estimate().Rows {
		t.Errorf("Hash estimate = %+v, want startup = total and the input's rows", h)
	}
	// Four relations plan without complaint.
	if _, err := Plan(analyze(t, "select 1 from t, u, small, big where t.a = u.a and u.a = small.a and small.a = big.a")); err != nil {
		t.Errorf("four relations: %v", err)
	}
}
