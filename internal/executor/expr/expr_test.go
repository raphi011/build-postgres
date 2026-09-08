package expr

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/sql/analyzer"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/parser"
	"github.com/raphi011/build-postgres/internal/sql/query"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// fakeCatalog is an in-memory analyzer.Catalog.
type fakeCatalog map[string]*catalog.RelationInfo

func (f fakeCatalog) Lookup(name string) (*catalog.RelationInfo, error) {
	if info, ok := f[name]; ok {
		return info, nil
	}
	return nil, catalog.ErrNotFound
}

// Two tables, so a row built FROM t, u has six slots:
// t.i=0 t.j=1 t.s=2 t.b=3 u.n=4 u.s=5.
var cat = fakeCatalog{
	"t": {OID: 16384, Name: "t", Kind: catalog.RelKindTable, Desc: tuple.NewDesc(
		tuple.Attr{Name: "i", Type: tuple.Int4},
		tuple.Attr{Name: "j", Type: tuple.Int4},
		tuple.Attr{Name: "s", Type: tuple.Text},
		tuple.Attr{Name: "b", Type: tuple.Bool},
	)},
	"u": {OID: 16385, Name: "u", Kind: catalog.RelKindTable, Desc: tuple.NewDesc(
		tuple.Attr{Name: "n", Type: tuple.Int8},
		tuple.Attr{Name: "s", Type: tuple.Text},
	)},
}

// compileSelect analyzes a SELECT and compiles its first target.
func compileSelect(t *testing.T, sql string) (Func, Layout) {
	t.Helper()
	stmts, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("Parse(%q): %v", sql, err)
	}
	q, err := analyzer.Analyze(stmts[0], cat)
	if err != nil {
		t.Fatalf("Analyze(%q): %v", sql, err)
	}
	sel := q.(*query.Select)
	l := NewLayout(sel.Range)
	return Compile(sel.Targets[0].Expr, l), l
}

// eval evaluates a table-free expression.
func eval(t *testing.T, src string) (tuple.Datum, bool, error) {
	t.Helper()
	f, _ := compileSelect(t, "select "+src)
	return f(Row{})
}

// evalRow evaluates an expression over t, u against row.
func evalRow(t *testing.T, src string, row Row) (tuple.Datum, bool, error) {
	t.Helper()
	f, l := compileSelect(t, "select "+src+" from t, u")
	if len(row.Values) != l.Width() {
		t.Fatalf("row has %d slots, layout wants %d", len(row.Values), l.Width())
	}
	return f(row)
}

// row builds a Row where a nil value is NULL.
func row(values ...tuple.Datum) Row {
	r := Row{Values: values, Nulls: make([]bool, len(values))}
	for i, v := range values {
		r.Nulls[i] = v == nil
	}
	return r
}

// want checks a successful evaluation. A nil want means NULL.
func want(t *testing.T, src string, v tuple.Datum, null bool, err error, want tuple.Datum) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: unexpected error %v", src, err)
		return
	}
	if want == nil {
		if !null {
			t.Errorf("%s = %#v, want NULL", src, v)
		}
		return
	}
	if null {
		t.Errorf("%s = NULL, want %#v", src, want)
		return
	}
	if v != want {
		t.Errorf("%s = %#v (%T), want %#v (%T)", src, v, v, want, want)
	}
}

func TestLayout(t *testing.T) {
	_, l := compileSelect(t, "select 1 from t, u")
	if got := []int(l); len(got) != 3 || got[0] != 0 || got[1] != 4 || got[2] != 6 {
		t.Fatalf("Layout = %v, want [0 4 6]", got)
	}
	if l.Width() != 6 {
		t.Errorf("Width = %d, want 6", l.Width())
	}
	if s := l.Slot(&query.Var{Rel: 1, Attr: 1}); s != 5 {
		t.Errorf("Slot(u.s) = %d, want 5", s)
	}
	if s := l.Slot(&query.Var{Rel: 0, Attr: 2}); s != 2 {
		t.Errorf("Slot(t.s) = %d, want 2", s)
	}
	empty := NewLayout(nil)
	if empty.Width() != 0 {
		t.Errorf("empty layout width = %d, want 0", empty.Width())
	}
}

func TestConstants(t *testing.T) {
	cases := []struct {
		src  string
		want tuple.Datum
	}{
		{"1", int32(1)},
		{"-2147483648", int32(math.MinInt32)},
		{"3000000000", int64(3000000000)},
		{"'x'", "x"},
		{"''", ""},
		{"true", true},
		{"false", false},
		{"null", nil},
	}
	for _, c := range cases {
		v, null, err := eval(t, c.src)
		want(t, c.src, v, null, err, c.want)
	}
}

func TestVars(t *testing.T) {
	r := row(int32(7), nil, "seven", true, int64(1)<<40, nil)
	cases := []struct {
		src  string
		want tuple.Datum
	}{
		{"i", int32(7)},
		{"j", nil},
		{"t.s", "seven"},
		{"b", true},
		{"n", int64(1) << 40},
		{"u.s", nil},
	}
	for _, c := range cases {
		v, null, err := evalRow(t, c.src, r)
		want(t, c.src, v, null, err, c.want)
	}
}

func TestArithmetic(t *testing.T) {
	cases := []struct {
		src  string
		want tuple.Datum
	}{
		{"1 + 2", int32(3)},
		{"5 - 7", int32(-2)},
		{"6 * 7", int32(42)},
		{"7 / 2", int32(3)},
		{"-7 / 2", int32(-3)},
		{"7 / -2", int32(-3)},
		{"-(-5)", int32(5)},
		{"2147483647 + 0", int32(math.MaxInt32)},
		{"-2147483648 + 0", int32(math.MinInt32)},
		{"1 + 2 * 3 - 4 / 2", int32(5)},
		{"3000000000 + 1", int64(3000000001)},
		{"3000000000 * 3", int64(9000000000)},
		{"9223372036854775807 - 0", int64(math.MaxInt64)},
		{"-9223372036854775808 / 2", int64(math.MinInt64 / 2)},
		{"-(-9223372036854775807)", int64(math.MaxInt64)},
		{"1 + null", nil},
		{"null * 2", nil},
		{"null / 0", nil},
		{"3000000000 - null", nil},
	}
	for _, c := range cases {
		v, null, err := eval(t, c.src)
		want(t, c.src, v, null, err, c.want)
	}
}

func TestArithmeticVars(t *testing.T) {
	r := row(int32(10), int32(3), "", false, int64(1)<<32, nil)
	cases := []struct {
		src  string
		want tuple.Datum
	}{
		{"i + j", int32(13)},
		{"i / j", int32(3)},
		{"-j", int32(-3)},
		{"i + n", int64(1)<<32 + 10}, // i is cast to int8
		{"n - i", int64(1)<<32 - 10},
		{"i * n", int64(10) << 32},
	}
	for _, c := range cases {
		v, null, err := evalRow(t, c.src, r)
		want(t, c.src, v, null, err, c.want)
	}
	// A NULL int4 cast to int8 stays NULL.
	v, null, err := evalRow(t, "j + n", row(int32(1), nil, "", false, int64(1), nil))
	want(t, "j + n", v, null, err, nil)
}

func TestErrors(t *testing.T) {
	cases := []struct {
		src string
		err error
		msg string
	}{
		{"2147483647 + 1", ErrOutOfRange, "integer out of range"},
		{"-2147483648 - 1", ErrOutOfRange, "integer out of range"},
		{"65536 * 65536", ErrOutOfRange, "integer out of range"},
		{"-65536 * 65536 - 1", ErrOutOfRange, "integer out of range"},
		{"-2147483648 / -1", ErrOutOfRange, "integer out of range"},
		{"-(-2147483648)", ErrOutOfRange, "integer out of range"},
		{"9223372036854775807 + 1", ErrOutOfRange, "bigint out of range"},
		{"-9223372036854775808 - 1", ErrOutOfRange, "bigint out of range"},
		{"4294967296 * 4294967296", ErrOutOfRange, "bigint out of range"},
		{"-9223372036854775808 / -1", ErrOutOfRange, "bigint out of range"},
		{"-(-9223372036854775808)", ErrOutOfRange, "bigint out of range"},
		{"1 / 0", ErrDivisionByZero, "division by zero"},
		{"1 / (1 - 1)", ErrDivisionByZero, "division by zero"},
		{"3000000000 / 0", ErrDivisionByZero, "division by zero"},
		{"(1 / 0) + null", ErrDivisionByZero, "division by zero"},
		{"1 / 0 = 1", ErrDivisionByZero, "division by zero"},
		{"(1 / 0) is null", ErrDivisionByZero, "division by zero"},
	}
	for _, c := range cases {
		_, _, err := eval(t, c.src)
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%s: error %v (%T), want *Error", c.src, err, err)
			continue
		}
		if !errors.Is(err, c.err) {
			t.Errorf("%s: error %v does not unwrap to %v", c.src, err, c.err)
		}
		if e.Msg != c.msg || err.Error() != c.msg {
			t.Errorf("%s: message %q, want %q", c.src, e.Msg, c.msg)
		}
	}
	// Overflow from a variable, not a folded literal.
	_, _, err := evalRow(t, "-i", row(int32(math.MinInt32), nil, "", false, int64(0), nil))
	if !errors.Is(err, ErrOutOfRange) {
		t.Errorf("-i with MinInt32: %v, want ErrOutOfRange", err)
	}
	_, _, err = evalRow(t, "n * n", row(int32(0), nil, "", false, int64(1)<<32, nil))
	if !errors.Is(err, ErrOutOfRange) {
		t.Errorf("n * n with 2^32: %v, want ErrOutOfRange", err)
	}
}

func TestComparison(t *testing.T) {
	cases := []struct {
		src  string
		want tuple.Datum
	}{
		{"1 = 1", true},
		{"1 = 2", false},
		{"1 <> 1", false},
		{"1 <> 2", true},
		{"1 < 2", true},
		{"2 < 2", false},
		{"2 <= 2", true},
		{"3 <= 2", false},
		{"1 > 2", false},
		{"2 > 1", true},
		{"2 >= 2", true},
		{"1 >= 2", false},
		{"-1 < 1", true},
		// int8, and int4 widened against int8.
		{"3000000000 > 1", true},
		{"1 < 3000000000", true},
		{"3000000000 = 3000000000", true},
		{"-9223372036854775808 < 9223372036854775807", true},
		// bool: false sorts before true.
		{"false < true", true},
		{"true < false", false},
		{"true = true", true},
		{"false = true", false},
		{"true >= false", true},
		{"false <> true", true},
		// text: bytewise, so upper case before lower case and a prefix first.
		{"'a' = 'a'", true},
		{"'a' = 'A'", false},
		{"'a' < 'b'", true},
		{"'B' < 'a'", true},
		{"'ab' < 'abc'", true},
		{"'abc' < 'ab'", false},
		{"'' < 'a'", true},
		{"'abc' <> 'abd'", true},
		{"'é' > 'z'", true},
		{"'a' >= 'a'", true},
		{"'b' <= 'a'", false},
		// Comparison against an untyped literal parses the literal.
		{"1 = '1'", true},
		{"'true' = true", true},
		// NULL makes every comparison NULL.
		{"null = null", nil},
		{"1 = null", nil},
		{"null <> 1", nil},
		{"'a' < null", nil},
		{"null > true", nil},
		{"3000000000 >= null", nil},
	}
	for _, c := range cases {
		v, null, err := eval(t, c.src)
		want(t, c.src, v, null, err, c.want)
	}
}

func TestComparisonVars(t *testing.T) {
	r := row(int32(5), nil, "x", true, int64(5), "x")
	cases := []struct {
		src  string
		want tuple.Datum
	}{
		{"i = 5", true},
		{"i = n", true}, // i cast to int8
		{"t.s = u.s", true},
		{"i < j", nil},
		{"j = j", nil},
		{"b = true", true},
	}
	for _, c := range cases {
		v, null, err := evalRow(t, c.src, r)
		want(t, c.src, v, null, err, c.want)
	}
}

// TestThreeValuedLogic checks the full truth tables of AND, OR, and NOT.
func TestThreeValuedLogic(t *testing.T) {
	vals := []string{"true", "false", "null"}
	and := map[[2]string]tuple.Datum{
		{"true", "true"}: true, {"true", "false"}: false, {"true", "null"}: nil,
		{"false", "true"}: false, {"false", "false"}: false, {"false", "null"}: false,
		{"null", "true"}: nil, {"null", "false"}: false, {"null", "null"}: nil,
	}
	or := map[[2]string]tuple.Datum{
		{"true", "true"}: true, {"true", "false"}: true, {"true", "null"}: true,
		{"false", "true"}: true, {"false", "false"}: false, {"false", "null"}: nil,
		{"null", "true"}: true, {"null", "false"}: nil, {"null", "null"}: nil,
	}
	not := map[string]tuple.Datum{"true": false, "false": true, "null": nil}
	for _, a := range vals {
		for _, b := range vals {
			src := a + " and " + b
			v, null, err := eval(t, src)
			want(t, src, v, null, err, and[[2]string{a, b}])
			src = a + " or " + b
			v, null, err = eval(t, src)
			want(t, src, v, null, err, or[[2]string{a, b}])
		}
		src := "not " + a
		v, null, err := eval(t, src)
		want(t, src, v, null, err, not[a])
	}
	// Flattened chains.
	cases := []struct {
		src  string
		want tuple.Datum
	}{
		{"true and true and null", nil},
		{"true and null and false", false},
		{"false or null or true", true},
		{"false or null or false", nil},
		{"not (true and null)", nil},
		{"not (false or null)", nil},
		{"(null or true) and (null or false)", nil},
	}
	for _, c := range cases {
		v, null, err := eval(t, c.src)
		want(t, c.src, v, null, err, c.want)
	}
}

func TestNullTest(t *testing.T) {
	cases := []struct {
		src  string
		want tuple.Datum
	}{
		{"null is null", true},
		{"null is not null", false},
		{"1 is null", false},
		{"1 is not null", true},
		{"'' is null", false},
		{"(1 = null) is null", true},
		{"(null and false) is null", false},
	}
	for _, c := range cases {
		v, null, err := eval(t, c.src)
		want(t, c.src, v, null, err, c.want)
	}
	r := row(int32(1), nil, "", false, int64(0), nil)
	v, null, err := evalRow(t, "j is null", r)
	want(t, "j is null", v, null, err, true)
	v, null, err = evalRow(t, "i is null", r)
	want(t, "i is null", v, null, err, false)
}

// TestShortCircuit: AND stops at the first FALSE and OR at the first TRUE,
// so a later argument is never evaluated. NULL does not stop evaluation.
func TestShortCircuit(t *testing.T) {
	ok := []struct {
		src  string
		want tuple.Datum
	}{
		{"false and (1 / 0 = 0)", false},
		{"true or (1 / 0 = 0)", true},
		{"true and false and (1 / 0 = 0)", false},
		{"null or true or (1 / 0 = 0)", true},
	}
	for _, c := range ok {
		v, null, err := eval(t, c.src)
		want(t, c.src, v, null, err, c.want)
	}
	fails := []string{
		"(1 / 0 = 0) and false",
		"(1 / 0 = 0) or true",
		"null and (1 / 0 = 0)",
		"null or (1 / 0 = 0)",
		"true and (1 / 0 = 0)",
		"not (1 / 0 = 0)",
	}
	for _, src := range fails {
		_, _, err := eval(t, src)
		if !errors.Is(err, ErrDivisionByZero) {
			t.Errorf("%s: %v, want ErrDivisionByZero", src, err)
		}
	}
}

func TestQual(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"null", false},
		{"1 = null", false},
		{"1 = 1", true},
		{"null or true", true},
	}
	for _, c := range cases {
		f, _ := compileSelect(t, "select "+c.src)
		got, err := f.Qual(Row{})
		if err != nil {
			t.Errorf("%s: %v", c.src, err)
			continue
		}
		if got != c.want {
			t.Errorf("Qual(%s) = %v, want %v", c.src, got, c.want)
		}
	}
	f, _ := compileSelect(t, "select 1 / 0 = 0")
	if _, err := f.Qual(Row{}); !errors.Is(err, ErrDivisionByZero) {
		t.Errorf("Qual(1 / 0 = 0): %v, want ErrDivisionByZero", err)
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b tuple.Datum
		want int
	}{
		{int32(1), int32(2), -1},
		{int32(2), int32(2), 0},
		{int32(3), int32(2), 1},
		{int32(math.MinInt32), int32(math.MaxInt32), -1},
		{int64(1), int64(2), -1},
		{int64(math.MaxInt64), int64(math.MinInt64), 1},
		{int64(5), int64(5), 0},
		{false, true, -1},
		{true, false, 1},
		{true, true, 0},
		{"a", "b", -1},
		{"b", "a", 1},
		{"a", "a", 0},
		{"B", "a", -1},
		{"ab", "abc", -1},
		{"", "a", -1},
		{"é", "z", 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%#v, %#v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
	// Sorting with Compare gives byte order (C collation), not
	// dictionary order.
	words := []string{"banana", "Apple", "apple", "b", "Banana", "", "zebra", "Zebra", "äpple"}
	sort.Slice(words, func(i, j int) bool { return Compare(words[i], words[j]) < 0 })
	got := strings.Join(words, ",")
	wantOrder := ",Apple,Banana,Zebra,apple,b,banana,zebra,äpple"
	if got != wantOrder {
		t.Errorf("sorted = %s\n   want = %s", got, wantOrder)
	}
}

// TestArithmeticProperty compares int4 and int8 arithmetic on random
// operands against big.Int, including which cases must overflow.
func TestArithmeticProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(10))
	ops := []ast.BinOp{ast.Add, ast.Sub, ast.Mul, ast.Div}
	ints := []struct {
		typ      tuple.TypeID
		min, max *big.Int
		random   func() tuple.Datum
		toBig    func(tuple.Datum) *big.Int
	}{
		{tuple.Int4, big.NewInt(math.MinInt32), big.NewInt(math.MaxInt32),
			func() tuple.Datum { return randInt32(rng) },
			func(d tuple.Datum) *big.Int { return big.NewInt(int64(d.(int32))) }},
		{tuple.Int8, big.NewInt(math.MinInt64), big.NewInt(math.MaxInt64),
			func() tuple.Datum { return randInt64(rng) },
			func(d tuple.Datum) *big.Int { return big.NewInt(d.(int64)) }},
	}
	for _, it := range ints {
		for i := 0; i < 20000; i++ {
			a, b := it.random(), it.random()
			op := ops[rng.Intn(len(ops))]
			e := &query.OpExpr{Op: op, Typ: it.typ,
				Left: &query.Const{Typ: it.typ, Value: a}, Right: &query.Const{Typ: it.typ, Value: b}}
			v, null, err := Compile(e, nil)(Row{})
			ba, bb := it.toBig(a), it.toBig(b)
			var exp *big.Int
			switch op {
			case ast.Add:
				exp = new(big.Int).Add(ba, bb)
			case ast.Sub:
				exp = new(big.Int).Sub(ba, bb)
			case ast.Mul:
				exp = new(big.Int).Mul(ba, bb)
			case ast.Div:
				if bb.Sign() == 0 {
					if !errors.Is(err, ErrDivisionByZero) {
						t.Fatalf("%v: %v, want ErrDivisionByZero", e, err)
					}
					continue
				}
				exp = new(big.Int).Quo(ba, bb) // truncates toward zero
			}
			if exp.Cmp(it.min) < 0 || exp.Cmp(it.max) > 0 {
				if !errors.Is(err, ErrOutOfRange) {
					t.Fatalf("%v: got %v/%v, want ErrOutOfRange", e, v, err)
				}
				continue
			}
			if err != nil || null {
				t.Fatalf("%v: null=%v err=%v, want %v", e, null, err, exp)
			}
			if it.toBig(v).Cmp(exp) != 0 {
				t.Fatalf("%v = %v, want %v", e, v, exp)
			}
		}
	}
}

// randInt32 favours the edges of the range so overflow cases are common.
func randInt32(rng *rand.Rand) int32 {
	switch rng.Intn(4) {
	case 0:
		return int32(rng.Intn(21) - 10)
	case 1:
		return math.MaxInt32 - int32(rng.Intn(3))
	case 2:
		return math.MinInt32 + int32(rng.Intn(3))
	}
	return int32(rng.Uint32())
}

func randInt64(rng *rand.Rand) int64 {
	switch rng.Intn(4) {
	case 0:
		return int64(rng.Intn(21) - 10)
	case 1:
		return math.MaxInt64 - int64(rng.Intn(3))
	case 2:
		return math.MinInt64 + int64(rng.Intn(3))
	}
	return int64(rng.Uint64())
}
