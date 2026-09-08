// Package index keeps a table's B-tree indexes in step with its heap: it
// builds an index over existing tuples, adds entries for new ones, and
// enforces unique indexes. See chapters/13-indexes.
package index

import (
	"errors"

	"github.com/raphi011/build-postgres/internal/btree"
	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// Sentinel errors, one per PostgreSQL error class.
var (
	ErrUniqueViolation = errors.New("unique violation")       // 23505
	ErrTooLarge        = errors.New("program limit exceeded") // 54000
)

// Error is an index error with PostgreSQL's message text.
type Error struct {
	Err error
	Msg string
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Err }

// Open returns a handle on idx, an index of rel, keyed by the type of the
// indexed column. It does no I/O.
// PostgreSQL: index_open in indexam.c.
func Open(pool *bufmgr.Pool, rel *catalog.RelationInfo, idx *catalog.IndexInfo) *btree.Tree {
	panic("not implemented")
}

// Check verifies that a row with values vals (nulls marking NULLs) can
// be indexed in every index of rel, so that Insert cannot fail after the
// heap tuple is written: every key forms an index tuple no larger than
// btree.MaxItemSize, and CheckUnique passes. A key too large is a *Error
// wrapping ErrTooLarge with the message `index row size N exceeds btree
// version 4 maximum 2704 for index "name"`, N being the tuple's size.
// Nothing is written.
// PostgreSQL: _bt_check_third_page in nbtutils.c, ExecInsertIndexTuples
// in execIndexing.c.
func Check(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, except tuple.TID) error {
	panic("not implemented")
}

// CheckUnique verifies that a row with values vals (nulls marking NULLs)
// can be stored in rel without violating a unique index: for every
// unique index of rel, no live heap tuple other than the one at except
// (the zero TID for an insert, the old version for an update) has the
// same key. A NULL key never conflicts. Returns a *Error wrapping
// ErrUniqueViolation with the message
// `duplicate key value violates unique constraint "name"`.
// PostgreSQL: _bt_check_unique in nbtinsert.c.
func CheckUnique(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, except tuple.TID) error {
	panic("not implemented")
}

// Insert adds an entry pointing at tid to every index of rel for the row
// vals; a NULL key is indexed as NULL. Nothing is checked: call Check
// before writing the heap tuple.
// PostgreSQL: ExecInsertIndexTuples in execIndexing.c.
func Insert(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, tid tuple.TID) error {
	panic("not implemented")
}

// Build fills the empty index idx from every visible tuple of rel, in
// heap order. For a unique index two tuples with the same non-NULL key
// stop the build with a *Error wrapping ErrUniqueViolation and the
// message `could not create unique index "name"`; a key too large stops
// it with the ErrTooLarge error of Check. The entries inserted so far
// stay in the file, and the caller drops the index.
// PostgreSQL: index_build and _bt_load in nbtsort.c.
func Build(pool *bufmgr.Pool, rel *catalog.RelationInfo, idx *catalog.IndexInfo) error {
	panic("not implemented")
}
