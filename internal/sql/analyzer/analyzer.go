// Package analyzer binds a syntax tree to the catalog: it resolves table
// and column names, checks and assigns types, and produces a query.Stmt.
// See chapters/09-analyzer.
package analyzer

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/sql/ast"
	"github.com/raphi011/build-postgres/internal/sql/lexer"
	"github.com/raphi011/build-postgres/internal/sql/query"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// Catalog is what the analyzer needs from the system catalog. Lookup
// returns catalog.ErrNotFound for an unknown table.
type Catalog interface {
	Lookup(name string) (*catalog.RelationInfo, error)
}

// Sentinel errors, one per PostgreSQL error class the analyzer raises.
var (
	ErrUndefinedTable    = errors.New("undefined table")          // 42P01
	ErrUndefinedColumn   = errors.New("undefined column")         // 42703
	ErrAmbiguousColumn   = errors.New("ambiguous column")         // 42702
	ErrDuplicateAlias    = errors.New("duplicate alias")          // 42712
	ErrDuplicateColumn   = errors.New("duplicate column")         // 42701
	ErrUndefinedOperator = errors.New("undefined operator")       // 42883
	ErrTypeMismatch      = errors.New("datatype mismatch")        // 42804
	ErrInvalidColumnRef  = errors.New("invalid column ref")       // 42P10
	ErrInvalidInput      = errors.New("invalid input syntax")     // 22P02
	ErrOutOfRange        = errors.New("out of range")             // 22003
	ErrNotNull           = errors.New("not-null violation")       // 23502
	ErrSyntax            = errors.New("syntax error")             // 42601
	ErrTableDefinition   = errors.New("invalid table definition") // 42P16
)

// Error is an analysis error. Msg is PostgreSQL's message text. Pos is
// where the REPL should point; Pos.Line == 0 means there is no position.
type Error struct {
	Pos lexer.Pos
	Err error
	Msg string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// errAt builds an Error at pos; noPos is the zero position.
func errAt(pos lexer.Pos, sentinel error, format string, args ...any) error {
	return &Error{Pos: pos, Err: sentinel, Msg: fmt.Sprintf(format, args...)}
}

var noPos lexer.Pos

// unknown marks an untyped literal during analysis. It never escapes.
const unknown tuple.TypeID = 0

// pgTypeName is how PostgreSQL names a type in messages.
func pgTypeName(t tuple.TypeID) string {
	switch t {
	case tuple.Int4:
		return "integer"
	case tuple.Int8:
		return "bigint"
	case tuple.Bool:
		return "boolean"
	case tuple.Text:
		return "text"
	}
	return "unknown"
}

// Analyze binds stmt against cat. Every failure is a *Error carrying the
// sentinel, PostgreSQL's message, and the position of the offending
// token (Pos.Line == 0 where PostgreSQL reports none: NOT NULL
// violations, multiple assignments, multiple primary keys). A catalog
// error other than catalog.ErrNotFound is returned unchanged. A nil or
// unknown statement is an error, not a panic.
// PostgreSQL: transformStmt in analyze.c.
func Analyze(stmt ast.Stmt, cat Catalog) (query.Stmt, error) {
	a := &analyzer{cat: cat}
	switch s := stmt.(type) {
	case *ast.Select:
		return a.selectStmt(s)
	case *ast.Insert:
		return a.insert(s)
	case *ast.Update:
		return a.update(s)
	case *ast.Delete:
		return a.delete(s)
	case *ast.CreateTable:
		return a.createTable(s)
	case *ast.DropTable:
		return &query.DropTable{Name: s.Name}, nil
	case *ast.Begin:
		return &query.Begin{}, nil
	case *ast.Commit:
		return &query.Commit{}, nil
	case *ast.Rollback:
		return &query.Rollback{}, nil
	case *ast.Explain:
		inner, err := Analyze(s.Stmt, cat)
		if err != nil {
			return nil, err
		}
		return &query.Explain{Stmt: inner}, nil
	}
	return nil, fmt.Errorf("analyzer: unsupported statement %T", stmt)
}

type analyzer struct {
	cat Catalog
}

// scope is the range table an expression is resolved against.
type scope struct {
	entries []*query.RangeEntry
	// from is the first entry a column reference may resolve against.
	// It is 0 everywhere but inside a join's ON clause, which sees only
	// the relations of its own join. The entries stay in one slice
	// because Var.Rel indexes the whole range table.
	from int
}

// addRel looks a table up and appends it to the range table.
func (a *analyzer) addRel(sc *scope, name, alias string, pos lexer.Pos) (*query.RangeEntry, error) {
	info, err := a.cat.Lookup(name)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil, errAt(pos, ErrUndefinedTable, "relation %q does not exist", name)
	}
	if err != nil {
		return nil, err
	}
	if alias == "" {
		alias = name
	}
	for _, e := range sc.entries {
		if e.Alias == alias {
			return nil, errAt(pos, ErrDuplicateAlias, "table name %q specified more than once", alias)
		}
	}
	entry := &query.RangeEntry{Alias: alias, Rel: info}
	sc.entries = append(sc.entries, entry)
	return entry, nil
}

// Statements.

func (a *analyzer) selectStmt(s *ast.Select) (query.Stmt, error) {
	sc := &scope{}
	q := &query.Select{}

	// FROM first, as transformFromClause runs before the target list.
	var quals []query.Expr
	for _, te := range s.From {
		if err := a.fromItem(sc, te, &quals); err != nil {
			return nil, err
		}
	}
	q.Range = sc.entries

	for _, item := range s.Items {
		if star, ok := item.Expr.(*ast.Star); ok {
			if len(sc.entries) == 0 {
				return nil, errAt(star.Loc, ErrSyntax, "SELECT * with no tables specified is not valid")
			}
			for i, e := range sc.entries {
				for j, attr := range e.Rel.Desc.Attrs {
					q.Targets = append(q.Targets, query.Target{Name: attr.Name, Expr: a.newVar(e, i, j)})
				}
			}
			continue
		}
		e, err := a.expr(sc, item.Expr)
		if err != nil {
			return nil, err
		}
		name := item.Alias
		if name == "" {
			if ref, ok := item.Expr.(*ast.ColumnRef); ok {
				name = ref.Name
			} else {
				name = "?column?"
			}
		}
		q.Targets = append(q.Targets, query.Target{Name: name, Expr: resolveUnknown(e)})
	}

	if s.Where != nil {
		w, err := a.boolClause(sc, s.Where, "WHERE")
		if err != nil {
			return nil, err
		}
		quals = append(quals, w)
	}
	q.Where = conjunction(quals)

	for _, item := range s.OrderBy {
		key, err := a.sortKey(sc, q, item.Expr)
		if err != nil {
			return nil, err
		}
		q.OrderBy = append(q.OrderBy, query.SortKey{Expr: key, Desc: item.Desc})
	}

	if s.Limit != nil {
		l, err := a.expr(sc, s.Limit)
		if err != nil {
			return nil, err
		}
		if hasVar(l) {
			return nil, errAt(s.Limit.Pos(), ErrInvalidColumnRef, "argument of LIMIT must not contain variables")
		}
		l, err = a.coerce(l, tuple.Int8, s.Limit.Pos())
		if errors.Is(err, ErrTypeMismatch) {
			return nil, errAt(s.Limit.Pos(), ErrTypeMismatch, "argument of LIMIT must be type bigint, not type %s", pgTypeName(l.Type()))
		}
		if err != nil {
			return nil, err
		}
		q.Limit = l
	}
	return q, nil
}

// fromItem adds a FROM item to the scope, collecting ON conditions.
func (a *analyzer) fromItem(sc *scope, te ast.TableExpr, quals *[]query.Expr) error {
	switch t := te.(type) {
	case *ast.TableRef:
		_, err := a.addRel(sc, t.Name, t.Alias, t.Loc)
		return err
	case *ast.Join:
		start := len(sc.entries)
		if err := a.fromItem(sc, t.Left, quals); err != nil {
			return err
		}
		if err := a.fromItem(sc, t.Right, quals); err != nil {
			return err
		}
		outer := sc.from
		sc.from = start
		on, err := a.boolClause(sc, t.On, "JOIN/ON")
		sc.from = outer
		if err != nil {
			return err
		}
		*quals = append(*quals, on)
		return nil
	}
	return fmt.Errorf("analyzer: unsupported table expression %T", te)
}

// boolClause analyzes a clause that must be boolean.
func (a *analyzer) boolClause(sc *scope, e ast.Expr, clause string) (query.Expr, error) {
	b, err := a.expr(sc, e)
	if err != nil {
		return nil, err
	}
	b, err = a.coerce(b, tuple.Bool, e.Pos())
	if errors.Is(err, ErrTypeMismatch) {
		return nil, errAt(e.Pos(), ErrTypeMismatch, "argument of %s must be type boolean, not type %s", clause, pgTypeName(b.Type()))
	}
	return b, err
}

// conjunction ANDs quals together, flattening nested ANDs.
func conjunction(quals []query.Expr) query.Expr {
	switch len(quals) {
	case 0:
		return nil
	case 1:
		return quals[0]
	}
	var args []query.Expr
	for _, q := range quals {
		if b, ok := q.(*query.BoolExpr); ok && b.Op == query.And {
			args = append(args, b.Args...)
		} else {
			args = append(args, q)
		}
	}
	return &query.BoolExpr{Op: query.And, Args: args}
}

// sortKey resolves one ORDER BY item: a select-list position, an output
// column name, or an expression.
func (a *analyzer) sortKey(sc *scope, q *query.Select, e ast.Expr) (query.Expr, error) {
	switch x := e.(type) {
	case *ast.IntLit:
		n, err := strconv.Atoi(x.Text)
		if err != nil || n < 1 || n > len(q.Targets) {
			return nil, errAt(x.Loc, ErrInvalidColumnRef, "ORDER BY position %s is not in select list", x.Text)
		}
		return q.Targets[n-1].Expr, nil
	case *ast.ColumnRef:
		if x.Table == "" {
			var found query.Expr
			for _, t := range q.Targets {
				if t.Name != x.Name {
					continue
				}
				if found != nil && found.String() != t.Expr.String() {
					return nil, errAt(x.Loc, ErrInvalidColumnRef, "ORDER BY %q is ambiguous", x.Name)
				}
				if found == nil {
					found = t.Expr
				}
			}
			if found != nil {
				return found, nil
			}
		}
	}
	key, err := a.expr(sc, e)
	if err != nil {
		return nil, err
	}
	return resolveUnknown(key), nil
}

func (a *analyzer) insert(s *ast.Insert) (query.Stmt, error) {
	sc := &scope{}
	rel, err := a.addRel(sc, s.Table, "", s.Loc)
	if err != nil {
		return nil, err
	}
	desc := rel.Rel.Desc

	// Map value positions to attributes.
	var attrs []int
	if s.Columns == nil {
		for i := range desc.Attrs {
			attrs = append(attrs, i)
		}
	} else {
		seen := map[int]bool{}
		for k, name := range s.Columns {
			i := attrIndex(desc, name)
			if i < 0 {
				return nil, errAt(s.ColumnLocs[k], ErrUndefinedColumn, "column %q of relation %q does not exist", name, s.Table)
			}
			if seen[i] {
				return nil, errAt(s.ColumnLocs[k], ErrDuplicateColumn, "column %q specified more than once", name)
			}
			seen[i] = true
			attrs = append(attrs, i)
		}
	}

	// The VALUES list is checked on its own, before any row is bound: a
	// short row would otherwise be padded with nulls and the user told
	// about a column they never wrote. PostgreSQL: transformValuesClause.
	for _, row := range s.Rows[1:] {
		if len(row) != len(s.Rows[0]) {
			return nil, errAt(row[len(row)-1].Pos(), ErrSyntax, "VALUES lists must all be the same length")
		}
	}

	// Values cannot see the target relation.
	empty := &scope{}
	q := &query.Insert{Rel: rel}
	for _, row := range s.Rows {
		if len(row) > len(attrs) {
			return nil, errAt(row[len(attrs)].Pos(), ErrSyntax, "INSERT has more expressions than target columns")
		}
		if s.Columns != nil && len(row) < len(attrs) {
			return nil, errAt(s.ColumnLocs[len(row)], ErrSyntax, "INSERT has more target columns than expressions")
		}
		bound := make([]query.Expr, len(desc.Attrs))
		for i, e := range row {
			v, err := a.assignValue(empty, e, desc.Attrs[attrs[i]], s.Table)
			if err != nil {
				return nil, err
			}
			bound[attrs[i]] = v
		}
		for i, attr := range desc.Attrs {
			if bound[i] != nil {
				continue
			}
			if attr.NotNull {
				return nil, notNull(attr.Name, s.Table)
			}
			bound[i] = &query.Const{Typ: attr.Type, Null: true}
		}
		q.Rows = append(q.Rows, bound)
	}
	return q, nil
}

func (a *analyzer) update(s *ast.Update) (query.Stmt, error) {
	sc := &scope{}
	rel, err := a.addRel(sc, s.Table, "", s.Loc)
	if err != nil {
		return nil, err
	}
	q := &query.Update{Rel: rel}
	seen := map[int]bool{}
	for _, as := range s.Set {
		i := attrIndex(rel.Rel.Desc, as.Column)
		if i < 0 {
			return nil, errAt(as.Loc, ErrUndefinedColumn, "column %q of relation %q does not exist", as.Column, s.Table)
		}
		if seen[i] {
			return nil, errAt(noPos, ErrSyntax, "multiple assignments to same column %q", as.Column)
		}
		seen[i] = true
		v, err := a.assignValue(sc, as.Value, rel.Rel.Desc.Attrs[i], s.Table)
		if err != nil {
			return nil, err
		}
		q.Set = append(q.Set, query.Assignment{Attr: i, Value: v})
	}
	if s.Where != nil {
		if q.Where, err = a.boolClause(sc, s.Where, "WHERE"); err != nil {
			return nil, err
		}
	}
	return q, nil
}

func (a *analyzer) delete(s *ast.Delete) (query.Stmt, error) {
	sc := &scope{}
	rel, err := a.addRel(sc, s.Table, "", s.Loc)
	if err != nil {
		return nil, err
	}
	q := &query.Delete{Rel: rel}
	if s.Where != nil {
		if q.Where, err = a.boolClause(sc, s.Where, "WHERE"); err != nil {
			return nil, err
		}
	}
	return q, nil
}

func (a *analyzer) createTable(s *ast.CreateTable) (query.Stmt, error) {
	q := &query.CreateTable{Name: s.Name, Desc: tuple.NewDesc(), PrimaryKey: -1}
	for i, c := range s.Columns {
		if c.PrimaryKey {
			if q.PrimaryKey >= 0 {
				return nil, errAt(noPos, ErrTableDefinition, "multiple primary keys for table %q are not allowed", s.Name)
			}
			q.PrimaryKey = i
		}
		q.Desc.Attrs = append(q.Desc.Attrs, tuple.Attr{Name: c.Name, Type: c.Type, NotNull: c.NotNull || c.PrimaryKey})
	}
	return q, nil
}

// assignValue analyzes a value stored into column attr of table.
func (a *analyzer) assignValue(sc *scope, e ast.Expr, attr tuple.Attr, table string) (query.Expr, error) {
	v, err := a.expr(sc, e)
	if err != nil {
		return nil, err
	}
	if c, ok := v.(*query.Const); ok && c.Null && attr.NotNull {
		return nil, notNull(attr.Name, table)
	}
	v, err = a.coerce(v, attr.Type, e.Pos())
	if errors.Is(err, ErrTypeMismatch) {
		return nil, errAt(e.Pos(), ErrTypeMismatch, "column %q is of type %s but expression is of type %s",
			attr.Name, pgTypeName(attr.Type), pgTypeName(v.Type()))
	}
	return v, err
}

func notNull(column, table string) error {
	return errAt(noPos, ErrNotNull, "null value in column %q of relation %q violates not-null constraint", column, table)
}

func attrIndex(desc *tuple.Desc, name string) int {
	for i, a := range desc.Attrs {
		if a.Name == name {
			return i
		}
	}
	return -1
}

// Expressions.

func (a *analyzer) newVar(e *query.RangeEntry, rel, attr int) *query.Var {
	col := e.Rel.Desc.Attrs[attr]
	return &query.Var{Rel: rel, Attr: attr, Typ: col.Type, Alias: e.Alias, Column: col.Name}
}

func (a *analyzer) expr(sc *scope, e ast.Expr) (query.Expr, error) {
	switch x := e.(type) {
	case *ast.IntLit:
		return intLiteral(x.Text, x.Loc)
	case *ast.StrLit:
		return &query.Const{Typ: unknown, Value: x.Value}, nil
	case *ast.BoolLit:
		return &query.Const{Typ: tuple.Bool, Value: x.Value}, nil
	case *ast.NullLit:
		return &query.Const{Typ: unknown, Null: true}, nil
	case *ast.ColumnRef:
		return a.columnRef(sc, x)
	case *ast.Star:
		return nil, errAt(x.Loc, ErrSyntax, "syntax error at or near \"*\"")
	case *ast.UnaryExpr:
		if lit, ok := x.X.(*ast.IntLit); ok && x.Op == ast.Neg {
			return intLiteral("-"+lit.Text, x.Loc)
		}
		operand, err := a.expr(sc, x.X)
		if err != nil {
			return nil, err
		}
		switch x.Op {
		case ast.Neg:
			operand = resolveUnknown(operand)
			if t := operand.Type(); t != tuple.Int4 && t != tuple.Int8 {
				return nil, errAt(x.Loc, ErrUndefinedOperator, "operator does not exist: - %s", pgTypeName(t))
			}
			return &query.Neg{X: operand}, nil
		case ast.Not:
			operand, err = a.boolArg(operand, "NOT", x.X.Pos())
			if err != nil {
				return nil, err
			}
			return &query.BoolExpr{Op: query.Not, Args: []query.Expr{operand}}, nil
		}
	case *ast.BinaryExpr:
		return a.binary(sc, x)
	case *ast.IsNull:
		operand, err := a.expr(sc, x.X)
		if err != nil {
			return nil, err
		}
		return &query.NullTest{X: resolveUnknown(operand), Not: x.Not}, nil
	}
	return nil, fmt.Errorf("analyzer: unsupported expression %T", e)
}

func (a *analyzer) columnRef(sc *scope, ref *ast.ColumnRef) (query.Expr, error) {
	if ref.Table != "" {
		for i, e := range sc.entries[sc.from:] {
			if e.Alias != ref.Table {
				continue
			}
			if j := attrIndex(e.Rel.Desc, ref.Name); j >= 0 {
				return a.newVar(e, sc.from+i, j), nil
			}
			return nil, errAt(ref.Loc, ErrUndefinedColumn, "column %s.%s does not exist", ref.Table, ref.Name)
		}
		for _, e := range sc.entries {
			if e.Rel.Name == ref.Table {
				return nil, errAt(ref.Loc, ErrUndefinedTable, "invalid reference to FROM-clause entry for table %q", ref.Table)
			}
		}
		return nil, errAt(ref.Loc, ErrUndefinedTable, "missing FROM-clause entry for table %q", ref.Table)
	}
	var found *query.Var
	for i, e := range sc.entries[sc.from:] {
		j := attrIndex(e.Rel.Desc, ref.Name)
		if j < 0 {
			continue
		}
		if found != nil {
			return nil, errAt(ref.Loc, ErrAmbiguousColumn, "column reference %q is ambiguous", ref.Name)
		}
		found = a.newVar(e, sc.from+i, j)
	}
	if found == nil {
		return nil, errAt(ref.Loc, ErrUndefinedColumn, "column %q does not exist", ref.Name)
	}
	return found, nil
}

func (a *analyzer) binary(sc *scope, x *ast.BinaryExpr) (query.Expr, error) {
	left, err := a.expr(sc, x.Left)
	if err != nil {
		return nil, err
	}
	right, err := a.expr(sc, x.Right)
	if err != nil {
		return nil, err
	}

	switch x.Op {
	case ast.And, ast.Or:
		op := query.And
		if x.Op == ast.Or {
			op = query.Or
		}
		if left, err = a.boolArg(left, op.String(), x.Left.Pos()); err != nil {
			return nil, err
		}
		if right, err = a.boolArg(right, op.String(), x.Right.Pos()); err != nil {
			return nil, err
		}
		var args []query.Expr
		for _, arg := range []query.Expr{left, right} {
			if b, ok := arg.(*query.BoolExpr); ok && b.Op == op {
				args = append(args, b.Args...)
			} else {
				args = append(args, arg)
			}
		}
		return &query.BoolExpr{Op: op, Args: args}, nil
	}

	// Untyped literals take the other side's type; two of them are text.
	switch {
	case left.Type() == unknown && right.Type() == unknown:
		left, right = resolveUnknown(left), resolveUnknown(right)
	case left.Type() == unknown:
		if left, err = a.coerce(left, right.Type(), x.Left.Pos()); err != nil {
			return nil, err
		}
	case right.Type() == unknown:
		if right, err = a.coerce(right, left.Type(), x.Right.Pos()); err != nil {
			return nil, err
		}
	}
	// Widen int4 to int8.
	switch {
	case left.Type() == tuple.Int4 && right.Type() == tuple.Int8:
		left, _ = a.coerce(left, tuple.Int8, x.Left.Pos())
	case left.Type() == tuple.Int8 && right.Type() == tuple.Int4:
		right, _ = a.coerce(right, tuple.Int8, x.Right.Pos())
	}

	noOp := func() error {
		return errAt(x.Loc, ErrUndefinedOperator, "operator does not exist: %s %s %s",
			pgTypeName(left.Type()), x.Op, pgTypeName(right.Type()))
	}
	if left.Type() != right.Type() {
		return nil, noOp()
	}
	typ := left.Type()
	switch x.Op {
	case ast.Add, ast.Sub, ast.Mul, ast.Div:
		if typ != tuple.Int4 && typ != tuple.Int8 {
			return nil, noOp()
		}
		return &query.OpExpr{Op: x.Op, Typ: typ, Left: left, Right: right}, nil
	default:
		return &query.OpExpr{Op: x.Op, Typ: tuple.Bool, Left: left, Right: right}, nil
	}
}

// boolArg coerces an operand of AND, OR, or NOT to boolean.
func (a *analyzer) boolArg(e query.Expr, op string, pos lexer.Pos) (query.Expr, error) {
	e, err := a.coerce(e, tuple.Bool, pos)
	if errors.Is(err, ErrTypeMismatch) {
		return nil, errAt(pos, ErrTypeMismatch, "argument of %s must be type boolean, not type %s", op, pgTypeName(e.Type()))
	}
	return e, err
}

// coerce converts e to type to: untyped literals are parsed, int4 widens
// to int8, everything else is ErrTypeMismatch. On a mismatch the returned
// expression is e, so callers can name its type.
func (a *analyzer) coerce(e query.Expr, to tuple.TypeID, pos lexer.Pos) (query.Expr, error) {
	if c, ok := e.(*query.Const); ok && c.Typ == unknown {
		if c.Null {
			return &query.Const{Typ: to, Null: true}, nil
		}
		v, err := parseLiteral(c.Value.(string), to, pos)
		if err != nil {
			return e, err
		}
		return &query.Const{Typ: to, Value: v}, nil
	}
	if e.Type() == to {
		return e, nil
	}
	if e.Type() == tuple.Int4 && to == tuple.Int8 {
		if c, ok := e.(*query.Const); ok {
			if c.Null {
				return &query.Const{Typ: to, Null: true}, nil
			}
			return &query.Const{Typ: to, Value: int64(c.Value.(int32))}, nil
		}
		return &query.Cast{X: e, Typ: to}, nil
	}
	return e, ErrTypeMismatch
}

// resolveUnknown turns an untyped literal that met no typed operand into
// text.
func resolveUnknown(e query.Expr) query.Expr {
	if c, ok := e.(*query.Const); ok && c.Typ == unknown {
		return &query.Const{Typ: tuple.Text, Value: c.Value, Null: c.Null}
	}
	return e
}

// intLiteral types an integer literal: int4 if it fits, else int8.
func intLiteral(text string, pos lexer.Pos) (query.Expr, error) {
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, errAt(pos, ErrOutOfRange, "value %q is out of range for type bigint", text)
	}
	if n >= -1<<31 && n < 1<<31 {
		return &query.Const{Typ: tuple.Int4, Value: int32(n)}, nil
	}
	return &query.Const{Typ: tuple.Int8, Value: n}, nil
}

// parseLiteral converts the text of an untyped literal to a value of
// type to, the way the type's input function would.
func parseLiteral(text string, to tuple.TypeID, pos lexer.Pos) (tuple.Datum, error) {
	switch to {
	case tuple.Text:
		return text, nil
	case tuple.Int4, tuple.Int8:
		bits := 32
		if to == tuple.Int8 {
			bits = 64
		}
		n, err := strconv.ParseInt(strings.TrimSpace(text), 10, bits)
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && errors.Is(numErr.Err, strconv.ErrRange) {
			return nil, errAt(pos, ErrOutOfRange, "value %q is out of range for type %s", text, pgTypeName(to))
		}
		if err != nil {
			return nil, errAt(pos, ErrInvalidInput, "invalid input syntax for type %s: %q", pgTypeName(to), text)
		}
		if to == tuple.Int4 {
			return int32(n), nil
		}
		return n, nil
	case tuple.Bool:
		if v, ok := parseBool(text); ok {
			return v, nil
		}
		return nil, errAt(pos, ErrInvalidInput, "invalid input syntax for type boolean: %q", text)
	}
	return nil, errAt(pos, ErrTypeMismatch, "cannot coerce to %s", pgTypeName(to))
}

// parseBool follows parse_bool_with_len: a case-insensitive prefix of
// true/false/yes/no/on/off, or 1/0, with surrounding whitespace ignored.
func parseBool(text string) (bool, bool) {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false, false
	}
	switch {
	case strings.HasPrefix("true", s), strings.HasPrefix("yes", s), s == "1":
		return true, true
	case strings.HasPrefix("false", s), strings.HasPrefix("no", s), s == "0":
		return false, true
	case strings.HasPrefix("on", s) && len(s) >= 2:
		return true, true
	case strings.HasPrefix("off", s) && len(s) >= 3:
		return false, true
	}
	return false, false
}

// hasVar reports whether e references a column.
func hasVar(e query.Expr) bool {
	switch x := e.(type) {
	case *query.Var:
		return true
	case *query.OpExpr:
		return hasVar(x.Left) || hasVar(x.Right)
	case *query.BoolExpr:
		for _, arg := range x.Args {
			if hasVar(arg) {
				return true
			}
		}
	case *query.Neg:
		return hasVar(x.X)
	case *query.NullTest:
		return hasVar(x.X)
	case *query.Cast:
		return hasVar(x.X)
	}
	return false
}
