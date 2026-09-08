// Package index keeps a table's B-tree indexes in step with its heap: it
// builds an index over existing tuples, adds entries for new ones, and
// enforces unique indexes. See chapters/13-indexes.
package index

import (
	"errors"
	"fmt"
	"sync"

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
// be indexed in every index of rel, so that the usual violation is
// reported before the heap tuple is written: every key forms an index
// tuple no larger than btree.MaxItemSize, and CheckUnique passes under
// snap. A key too large is a *Error wrapping ErrTooLarge with the message
// `index row size N exceeds btree version 4 maximum 2704 for index
// "name"`, N being the tuple's size. Nothing is written. Insert repeats
// the unique half of the check under a lock, because nothing stops
// another transaction from writing the key between the two.
// PostgreSQL: _bt_check_third_page in nbtutils.c, ExecInsertIndexTuples
// in execIndexing.c.
func Check(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, except tuple.TID, snap heap.Snapshot) error {
	for _, idx := range rel.Indexes {
		k := key(idx, vals, nulls)
		if k == nil {
			continue
		}
		if _, err := btree.FormIndexTuple(rel.Desc.Attrs[idx.Attr].Type, k, tuple.TID{}); err != nil {
			return tooLarge(err, idx, k)
		}
	}
	return CheckUnique(pool, rel, vals, nulls, except, snap)
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
// same key. Live is what snap's Dirty reports: a tuple another running
// transaction inserted or deleted makes the check wait for that
// transaction and look again. A NULL key never conflicts. With a nil
// snap a tuple is live while its xmax is zero. Returns a *Error wrapping
// ErrUniqueViolation with the message
// `duplicate key value violates unique constraint "name"`.
// PostgreSQL: _bt_check_unique in nbtinsert.c.
func CheckUnique(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, except tuple.TID, snap heap.Snapshot) error {
	for {
		wait, err := conflict(pool, rel, vals, nulls, except, snap)
		if err != nil || wait == tuple.InvalidXID {
			return err
		}
		snap.Wait(wait)
	}
}

// conflict looks once at every unique index of rel for a live tuple
// other than the one at except with the row's key. It returns the
// violation as an error, or the transaction to wait for before the
// question can be answered, and waits for nothing itself: Insert calls
// it holding a lock it must drop before waiting.
func conflict(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, except tuple.TID, snap heap.Snapshot) (tuple.XID, error) {
	h := heap.Open(pool, rel.OID, rel.Desc)
	for _, idx := range rel.Indexes {
		if !idx.Unique || nulls[idx.Attr] {
			continue
		}
		k := vals[idx.Attr]
		s := Open(pool, rel, idx).Scan(&btree.Bound{Key: k, Inclusive: true}, &btree.Bound{Key: k, Inclusive: true})
		dup, wait, err := liveEntry(s, h, except, snap)
		s.Close()
		if err != nil {
			return tuple.InvalidXID, err
		}
		if wait != tuple.InvalidXID {
			return wait, nil
		}
		if dup {
			return tuple.InvalidXID, &Error{Err: ErrUniqueViolation,
				Msg: fmt.Sprintf("duplicate key value violates unique constraint %q", idx.Name)}
		}
	}
	return tuple.InvalidXID, nil
}

// insertLocks holds one mutex per relation OID, taken by Insert across
// its unique check and its writes. PostgreSQL gets the same window from
// the write lock _bt_doinsert holds on the leaf page while
// _bt_check_unique reads it; there is no such lock here, because the
// check reads through a btree.Scan, which unpins every page it leaves.
// An entry stays for the life of the process, one mutex per relation
// ever written to.
var insertLocks = struct {
	mu sync.Mutex
	m  map[tuple.OID]*sync.Mutex
}{m: map[tuple.OID]*sync.Mutex{}}

// insertLock returns the insert lock of rel, creating it on first use.
func insertLock(rel tuple.OID) *sync.Mutex {
	insertLocks.mu.Lock()
	defer insertLocks.mu.Unlock()
	mu, ok := insertLocks.m[rel]
	if !ok {
		mu = &sync.Mutex{}
		insertLocks.m[rel] = mu
	}
	return mu
}

// liveEntry reports whether s yields an entry, other than except, whose
// heap tuple is live under snap, or the transaction to wait for before
// that can be known.
func liveEntry(s *btree.Scan, h *heap.Relation, except tuple.TID, snap heap.Snapshot) (conflict bool, wait tuple.XID, err error) {
	for s.Next() {
		tid := s.TID()
		if tid == except {
			continue
		}
		t, live, err := h.Fetch(tid, nil)
		if err != nil {
			return false, 0, err
		}
		if snap != nil {
			live, wait, err = snap.Dirty(t)
			if err != nil || wait != tuple.InvalidXID {
				return false, wait, err
			}
		}
		if live {
			return true, 0, nil
		}
	}
	return false, 0, s.Err()
}

// Insert adds an entry pointing at tid to every index of rel for the row
// vals; a NULL key is indexed as NULL. It holds rel's insert lock across
// a repeat of CheckUnique and the writes, so that the last look at a
// unique index and the entry that answers it are one critical section:
// two transactions that both pass Check cannot then both write the key.
// except is what it was for Check, the zero TID for an insert and the old
// version for an update. A conflict found here is CheckUnique's *Error,
// and the heap tuple the caller has already written is left for the
// transaction's abort to bury. A wait for another transaction happens
// with the lock released.
// PostgreSQL: ExecInsertIndexTuples in execIndexing.c, over a
// _bt_doinsert that holds the leaf page's write lock across
// _bt_check_unique.
func Insert(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, tid, except tuple.TID, snap heap.Snapshot) error {
	mu := insertLock(rel.OID)
	for {
		mu.Lock()
		wait, err := conflict(pool, rel, vals, nulls, except, snap)
		if err != nil {
			mu.Unlock()
			return err
		}
		if wait != tuple.InvalidXID {
			// Never wait holding the lock: the transaction waited for
			// may want it to finish its own insert.
			mu.Unlock()
			snap.Wait(wait)
			continue
		}
		err = insertEntries(pool, rel, vals, nulls, tid)
		mu.Unlock()
		return err
	}
}

// insertEntries adds the row's key to every index of rel.
func insertEntries(pool *bufmgr.Pool, rel *catalog.RelationInfo, vals []tuple.Datum, nulls []bool, tid tuple.TID) error {
	for _, idx := range rel.Indexes {
		if err := Open(pool, rel, idx).Insert(key(idx, vals, nulls), tid); err != nil {
			return err
		}
	}
	return nil
}

// Build fills the empty index idx from the tuples of rel, in heap
// order: every version some snapshot may still see, which is all of
// them but those whose inserting transaction aborted, so a deleted
// tuple is indexed too, and one a running transaction is inserting or
// deleting. For a unique index two live tuples (committed and not
// deleted, as snap's Dirty reports) with the same non-NULL key stop the
// build with a *Error wrapping ErrUniqueViolation and the message
// `could not create unique index "name"`; to know whether a tuple a
// running transaction is inserting or deleting counts, the build waits
// for that transaction. A key too large stops it with the ErrTooLarge
// error of Check. With a nil snap the tuples whose xmax is zero are
// indexed and all count as live. The entries inserted so far stay in
// the file, and the caller drops the index.
// PostgreSQL: heapam_index_build_range_scan in heapam_handler.c,
// _bt_load in nbtsort.c.
func Build(pool *bufmgr.Pool, rel *catalog.RelationInfo, idx *catalog.IndexInfo, snap heap.Snapshot) error {
	tree := Open(pool, rel, idx)
	seen := map[tuple.Datum]bool{}
	scanWith := snap
	if snap != nil {
		scanWith = heap.Any
	}
	h := heap.Open(pool, rel.OID, rel.Desc)
	s := h.Scan(scanWith)
	defer s.Close()
	for s.Next() {
		t := s.Tuple()
		live := true
		if snap != nil {
			index, isLive, err := buildTuple(h, snap, t, s.TID(), idx.Unique)
			if err != nil {
				return err
			}
			if !index {
				continue
			}
			live = isLive
		}
		vals, nulls, err := tuple.Deform(rel.Desc, t)
		if err != nil {
			return err
		}
		k := key(idx, vals, nulls)
		if idx.Unique && k != nil && live {
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

// buildTuple says whether an index build indexes the tuple t at tid and
// whether it counts as live for the unique check. A tuple whose
// inserting transaction aborted is skipped; a deleted one is indexed but
// not live. One a running transaction is inserting or deleting is
// indexed but not live for a plain index; for a unique one the build
// waits for that transaction and reads the tuple again.
func buildTuple(h *heap.Relation, snap heap.Snapshot, t tuple.Tuple, tid tuple.TID, unique bool) (index, live bool, err error) {
	for {
		live, wait, err := snap.Dirty(t)
		if err != nil {
			return false, false, err
		}
		if wait != tuple.InvalidXID {
			if !unique {
				return true, false, nil
			}
			snap.Wait(wait)
			if t, _, err = h.Fetch(tid, nil); err != nil {
				return false, false, err
			}
			continue
		}
		if live {
			return true, true, nil
		}
		res, err := snap.Modify(t, tid)
		if err != nil {
			return false, false, err
		}
		return res != heap.TMInvisible, false, nil
	}
}
