// Package index keeps a table's B-tree indexes in step with its heap: it
// builds an index over existing tuples, adds entries for new ones, and
// enforces unique indexes. See chapters/13-indexes.
package index

import (
	"errors"
	"fmt"

	"github.com/raphi011/build-postgres/internal/btree"
	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/catalog"
	"github.com/raphi011/build-postgres/internal/heap"
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
	return btree.Open(pool, idx.OID, rel.Desc.Attrs[idx.Attr].Type)
}

// key returns the index key of a row: nil for NULL.
func key(idx *catalog.IndexInfo, vals []tuple.Datum, nulls []bool) tuple.Datum {
	if nulls[idx.Attr] {
		return nil
	}
	return vals[idx.Attr]
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
	for _, idx := range rel.Indexes {
		k := key(idx, vals, nulls)
		if k == nil {
			continue
		}
		if _, err := btree.FormIndexTuple(rel.Desc.Attrs[idx.Attr].Type, k, tuple.TID{}); err != nil {
			return tooLarge(err, idx, k)
		}
	}
	return CheckUnique(pool, rel, vals, nulls, except)
}

// tooLarge turns btree.ErrKeyTooLarge into the ErrTooLarge error for
// key k of idx; any other error is returned as is.
func tooLarge(err error, idx *catalog.IndexInfo, k tuple.Datum) error {
	if !errors.Is(err, btree.ErrKeyTooLarge) {
		return err
	}
	size := btree.HeaderSize
	switch v := k.(type) {
	case int32:
		size += 4
	case int64:
		size += 8
	case bool:
		size++
	case string:
		size += 4 + len(v)
	}
	return &Error{Err: ErrTooLarge, Msg: fmt.Sprintf(
		"index row size %d exceeds btree version 4 maximum %d for index %q", size, btree.MaxItemSize, idx.Name)}
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
	h := heap.Open(pool, rel.OID, rel.Desc)
	for _, idx := range rel.Indexes {
		if !idx.Unique || nulls[idx.Attr] {
			continue
		}
		k := vals[idx.Attr]
		s := Open(pool, rel, idx).Scan(&btree.Bound{Key: k, Inclusive: true}, &btree.Bound{Key: k, Inclusive: true})
		conflict, err := liveEntry(s, h, except)
		s.Close()
		if err != nil {
			return err
		}
		if conflict {
			return &Error{Err: ErrUniqueViolation,
				Msg: fmt.Sprintf("duplicate key value violates unique constraint %q", idx.Name)}
		}
	}
	return nil
}

// liveEntry reports whether s yields an entry, other than except, whose
// heap tuple is live.
func liveEntry(s *btree.Scan, h *heap.Relation, except tuple.TID) (bool, error) {
	for s.Next() {
		tid := s.TID()
		if tid == except {
			continue
		}
		t, err := h.Fetch(tid)
		if err != nil {
			return false, err
		}
		if t.Xmax() == 0 {
			return true, nil
		}
	}
	return false, s.Err()
}

// Insert adds an entry pointing at tid to every index of rel for the row
// vals; a NULL key is indexed as NULL. Nothing is checked: call Check
// before writing the heap tuple.
// PostgreSQL: ExecInsertIndexTuples in execIndexing.c.
func Insert(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, tid tuple.TID) error {
	for _, idx := range rel.Indexes {
		if err := Open(pool, rel, idx).Insert(key(idx, vals, nulls), tid); err != nil {
			return err
		}
	}
	return nil
}

// Build fills the empty index idx from every visible tuple of rel, in
// heap order. For a unique index two tuples with the same non-NULL key
// stop the build with a *Error wrapping ErrUniqueViolation and the
// message `could not create unique index "name"`; a key too large stops
// it with the ErrTooLarge error of Check. The entries inserted so far
// stay in the file, and the caller drops the index.
// PostgreSQL: index_build and _bt_load in nbtsort.c.
func Build(pool *bufmgr.Pool, rel *catalog.RelationInfo, idx *catalog.IndexInfo) error {
	tree := Open(pool, rel, idx)
	seen := map[tuple.Datum]bool{}
	s := heap.Open(pool, rel.OID, rel.Desc).Scan()
	defer s.Close()
	for s.Next() {
		vals, nulls, err := tuple.Deform(rel.Desc, s.Tuple())
		if err != nil {
			return err
		}
		k := key(idx, vals, nulls)
		if idx.Unique && k != nil {
			if seen[k] {
				return &Error{Err: ErrUniqueViolation, Msg: fmt.Sprintf("could not create unique index %q", idx.Name)}
			}
			seen[k] = true
		}
		if err := tree.Insert(k, s.TID()); err != nil {
			return tooLarge(err, idx, k)
		}
	}
	return s.Err()
}
