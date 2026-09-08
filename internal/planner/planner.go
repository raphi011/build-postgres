// Package planner turns a bound statement into a plan tree. Until chapter
// 14 there is exactly one plan per statement. See chapters/11-executor.
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

// Plan builds the plan for a SELECT, INSERT, UPDATE, or DELETE. Returns
// ErrJoin for a query over more than one range entry and ErrUtility for
// any other statement kind.
// PostgreSQL: standard_planner in planner.c.
func Plan(q query.Stmt) (plan.Node, error) {
	switch q := q.(type) {
	case *query.Select:
		return planSelect(q)
	case *query.Insert:
		return &plan.ModifyTable{Op: plan.Insert, Rel: q.Rel, Input: &plan.Values{Rows: q.Rows}}, nil
	case *query.Update:
		return &plan.ModifyTable{Op: plan.Update, Rel: q.Rel, Input: scan(q.Rel, q.Where), Set: q.Set}, nil
	case *query.Delete:
		return &plan.ModifyTable{Op: plan.Delete, Rel: q.Rel, Input: scan(q.Rel, q.Where)}, nil
	}
	return nil, ErrUtility
}

func planSelect(q *query.Select) (plan.Node, error) {
	var n plan.Node
	switch len(q.Range) {
	case 0:
		n = &plan.Result{}
		if q.Where != nil {
			n = &plan.Filter{Input: n, Qual: q.Where}
		}
	case 1:
		n = scan(q.Range[0], q.Where)
	default:
		return nil, ErrJoin
	}
	if len(q.OrderBy) > 0 {
		n = &plan.Sort{Input: n, Keys: q.OrderBy}
	}
	n = &plan.Project{Input: n, Targets: q.Targets}
	if q.Limit != nil {
		n = &plan.Limit{Input: n, Count: q.Limit}
	}
	return n, nil
}

// scan reads rel, filtered by where when there is one.
func scan(rel *query.RangeEntry, where query.Expr) plan.Node {
	var n plan.Node = &plan.SeqScan{Rel: rel}
	if where != nil {
		n = &plan.Filter{Input: n, Qual: where}
	}
	return n
}
