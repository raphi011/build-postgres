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
	panic("not implemented")
}
