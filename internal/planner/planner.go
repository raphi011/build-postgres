// Package planner turns a bound statement into a plan tree, choosing
// scans, join methods, and the join order by estimated cost. See
// chapters/11-executor, chapters/14-planner, and chapters/15-joins.
package planner

import (
	"errors"

	"github.com/raphi011/build-postgres/internal/plan"
	"github.com/raphi011/build-postgres/internal/sql/query"
)

// ErrUtility is returned for statements the planner does not handle:
// DDL, transaction control, and EXPLAIN, which the session runs itself.
var ErrUtility = errors.New("planner: utility statement")

// ErrTooManyRelations is returned for a query whose FROM list is longer
// than MaxJoinRelations.
var ErrTooManyRelations = errors.New("too many range table entries")

// MaxJoinRelations caps the FROM list, because the join search
// enumerates every subset of it inside every subset: the work grows by
// about 3.5 times per relation, so thirteen already take seconds and
// sixteen are a hang with no way to interrupt it. PostgreSQL switches to
// a genetic search at geqo_threshold, the same number; here the query is
// refused instead.
const MaxJoinRelations = 12

// Cost parameters, PostgreSQL's defaults (costsize.c). A cost of 1 is
// one page read in sequence.
const (
	SeqPageCost       = 1.0
	RandomPageCost    = 4.0
	CPUTupleCost      = 0.01
	CPUIndexTupleCost = 0.005
	CPUOperatorCost   = 0.0025
)

// Selectivities for a column without statistics (selfuncs.h), which is
// every column here: there are no histograms (D21).
const (
	DefaultEqSel        = 0.005     // = and one side of a range
	DefaultIneqSel      = 1.0 / 3.0 // < <= > >= with one bound
	DefaultRangeIneqSel = 0.005     // a lower and an upper bound on one column
	DefaultUnkSel       = 0.005     // IS NULL
	DefaultBoolSel      = 0.5       // a boolean column
)

// DefaultPages is what a never-analysed relation is assumed to hold;
// its tuple count follows from the row width (estimate_rel_size).
const DefaultPages = 10

// DefaultNumDistinct is the number of distinct values assumed for a
// column of a table with at least that many rows, when nothing says
// otherwise (chapter 15, D22); it makes an equality join keep one row
// in 200.
const DefaultNumDistinct = 200

// Plan builds the cheapest plan for a SELECT, INSERT, UPDATE, or DELETE
// under the cost model of chapters 14 and 15. Every relation gets a
// sequential scan and one index scan per index that has a usable qual,
// provides the ORDER BY order, or can take a join qual as a parameter;
// every set of relations gets nested loop and hash join paths over
// every split into two smaller sets; a set keeps the paths that are
// not beaten on startup cost, total cost, order, and parameters; the
// whole plans built from the final set's paths are compared by total
// cost and the first cheapest wins. Every node's Estimate is set.
// Returns ErrTooManyRelations for a FROM list longer than
// MaxJoinRelations; ErrUtility for any other statement kind.
// PostgreSQL: standard_planner in planner.c, make_one_rel in allpaths.c,
// standard_join_search in joinrels.c, add_path in pathnode.c.
func Plan(q query.Stmt) (plan.Node, error) {
	panic("not implemented")
}
