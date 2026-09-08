// Package planner turns a bound statement into a plan tree, choosing
// between a sequential and an index scan by estimated cost. See
// chapters/11-executor and chapters/14-planner.
package planner

import (
	"errors"

	"github.com/raphi011/build-postgres/internal/plan"
	"github.com/raphi011/build-postgres/internal/sql/query"
)

var (
	// ErrUtility is returned for statements the planner does not handle:
	// DDL, transaction control, and EXPLAIN, which the session runs itself.
	ErrUtility = errors.New("planner: utility statement")
	// ErrJoin is returned for a query over more than one relation.
	ErrJoin = errors.New("joins are not supported until chapter 15")
)

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

// Plan builds the cheapest plan for a SELECT, INSERT, UPDATE, or DELETE
// under the cost model of chapter 14: for a query over one relation, a
// sequential scan and one index scan per index that has a usable qual
// or provides the ORDER BY order are costed as whole plans and the
// lowest total cost wins, a sequential scan on a tie. Every node's
// Estimate is set. Returns ErrJoin for a query over more than one range
// entry and ErrUtility for any other statement kind.
// PostgreSQL: standard_planner in planner.c, set_plain_rel_pathlist in
// allpaths.c, cost_seqscan and cost_index in costsize.c.
func Plan(q query.Stmt) (plan.Node, error) {
	panic("not implemented")
}
