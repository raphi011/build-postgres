// Package catalog stores the system catalogs pg_class, pg_attribute, and
// pg_index as heap relations and hands out object identifiers. See
// chapters/08-catalog and chapters/13-indexes.
package catalog

import (
	"errors"
	"sort"
	"sync"

	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/heap"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// Fixed OIDs. The catalog OIDs are PostgreSQL's own.
const (
	ClassOID     tuple.OID = 1259
	AttributeOID tuple.OID = 1249
	IndexOID     tuple.OID = 2610
	// FirstUserOID is the first OID handed to a user relation.
	FirstUserOID tuple.OID = 16384
)

// ClassDesc describes pg_class.
var ClassDesc = tuple.NewDesc(
	tuple.Attr{Name: "oid", Type: tuple.Int4, NotNull: true},
	tuple.Attr{Name: "relname", Type: tuple.Text, NotNull: true},
	tuple.Attr{Name: "relkind", Type: tuple.Text, NotNull: true},
	tuple.Attr{Name: "relpages", Type: tuple.Int4, NotNull: true},
	tuple.Attr{Name: "reltuples", Type: tuple.Int8, NotNull: true},
)

// AttributeDesc describes pg_attribute.
var AttributeDesc = tuple.NewDesc(
	tuple.Attr{Name: "attrelid", Type: tuple.Int4, NotNull: true},
	tuple.Attr{Name: "attname", Type: tuple.Text, NotNull: true},
	tuple.Attr{Name: "atttypid", Type: tuple.Int4, NotNull: true},
	tuple.Attr{Name: "attnum", Type: tuple.Int4, NotNull: true},
	tuple.Attr{Name: "attnotnull", Type: tuple.Bool, NotNull: true},
)

// IndexDesc describes pg_index: the index relation, the indexed table,
// the 1-based number of the key column, and the unique and primary key
// flags.
var IndexDesc = tuple.NewDesc(
	tuple.Attr{Name: "indexrelid", Type: tuple.Int4, NotNull: true},
	tuple.Attr{Name: "indrelid", Type: tuple.Int4, NotNull: true},
	tuple.Attr{Name: "indkey", Type: tuple.Int4, NotNull: true},
	tuple.Attr{Name: "indisunique", Type: tuple.Bool, NotNull: true},
	tuple.Attr{Name: "indisprimary", Type: tuple.Bool, NotNull: true},
)

var (
	ErrBootstrapped     = errors.New("catalog: data directory is already bootstrapped")
	ErrNotBootstrapped  = errors.New("catalog: data directory is not bootstrapped")
	ErrControlCorrupt   = errors.New("catalog: control file is corrupt")
	ErrExists           = errors.New("catalog: relation already exists")
	ErrNotFound         = errors.New("catalog: relation does not exist")
	ErrDuplicateColumn  = errors.New("catalog: column specified more than once")
	ErrSystemTable      = errors.New("catalog: cannot drop a system catalog")
	ErrWrongObjectType  = errors.New("catalog: relation is not of the required kind")
	ErrDependentObjects = errors.New("catalog: other objects depend on this one")
	ErrTooManyColumns   = errors.New("catalog: too many columns")
)

// MaxColumns is the widest table there can be, PostgreSQL's
// MaxHeapAttributeNumber. The tuple header stores the attribute count in
// 11 bits and the data offset in one byte, so a wider table would wrap
// both and return wrong values with no error at all.
// PostgreSQL: MaxHeapAttributeNumber in htup_details.h.
const MaxColumns = 1600

// RelKind is the relkind column of pg_class.
type RelKind byte

const (
	// RelKindTable is an ordinary heap table.
	RelKindTable RelKind = 'r'
	// RelKindIndex is a B-tree index relation.
	RelKindIndex RelKind = 'i'
)

// RelationInfo is a pg_class row together with the relation's columns
// and, for a table, its indexes. Values returned by Catalog are shared
// and must not be modified. An index relation has an empty Desc and no
// Indexes.
type RelationInfo struct {
	OID     tuple.OID
	Name    string
	Kind    RelKind
	Pages   int32
	Tuples  int64
	Desc    *tuple.Desc
	Indexes []*IndexInfo // primary key first, then by name
}

// IndexInfo is a pg_index row joined with the index relation's name:
// the index's own OID, the indexed table's OID, and the key column as a
// 0-based attribute number.
type IndexInfo struct {
	OID     tuple.OID
	Name    string
	Rel     tuple.OID
	Attr    int
	Unique  bool
	Primary bool
}

// Control is the content of <datadir>/global/control.
type Control struct {
	NextOID tuple.OID
	NextXID tuple.XID
}

// Catalog gives access to the system catalogs of one data directory. It is
// safe for concurrent use.
type Catalog struct {
	pool  *bufmgr.Pool
	class *heap.Relation
	attr  *heap.Relation

	mu      sync.Mutex
	control Control
	cache   map[string]*RelationInfo // relation cache by name
}

func newCatalog(pool *bufmgr.Pool, ctl Control) *Catalog {
	return &Catalog{
		pool:    pool,
		class:   heap.Open(pool, ClassOID, ClassDesc),
		attr:    heap.Open(pool, AttributeOID, AttributeDesc),
		control: ctl,
		cache:   map[string]*RelationInfo{},
	}
}

// Bootstrap initialises an empty data directory: it writes the control
// file, creates pg_class, pg_attribute, and pg_index, and inserts the
// rows that describe them (pg_class rows in that order, then the
// pg_attribute rows of each). Rows are stamped with tuple.BootstrapXID;
// the pool is flushed before returning. Returns ErrBootstrapped if a control file
// exists, ErrControlCorrupt if one exists but is unreadable.
// PostgreSQL: BootstrapModeMain in bootstrap.c.
func Bootstrap(pool *bufmgr.Pool) (*Catalog, error) {
	dir := pool.Store().Path()
	if _, err := ReadControl(dir); !errors.Is(err, ErrNotBootstrapped) {
		if err == nil {
			return nil, ErrBootstrapped
		}
		return nil, err
	}
	ctl := Control{NextOID: FirstUserOID, NextXID: tuple.FirstNormalXID}
	if err := WriteControl(dir, ctl); err != nil {
		return nil, err
	}
	if _, err := heap.Create(pool, ClassOID, ClassDesc); err != nil {
		return nil, err
	}
	if _, err := heap.Create(pool, AttributeOID, AttributeDesc); err != nil {
		return nil, err
	}
	c := newCatalog(pool, ctl)
	for _, rel := range []struct {
		oid  tuple.OID
		name string
	}{{ClassOID, "pg_class"}, {AttributeOID, "pg_attribute"}} {
		if err := c.insertClass(rel.oid, rel.name, tuple.BootstrapXID); err != nil {
			return nil, err
		}
	}
	if err := c.insertAttributes(ClassOID, ClassDesc, tuple.BootstrapXID); err != nil {
		return nil, err
	}
	if err := c.insertAttributes(AttributeOID, AttributeDesc, tuple.BootstrapXID); err != nil {
		return nil, err
	}
	if err := pool.FlushAll(); err != nil {
		return nil, err
	}
	return c, nil
}

// Open opens the catalogs of a bootstrapped data directory. Reads the
// control file and nothing else. Returns ErrNotBootstrapped if it is
// missing; ErrControlCorrupt if it is damaged.
// PostgreSQL: RelationCacheInitializePhase2 in relcache.c.
func Open(pool *bufmgr.Pool) (*Catalog, error) {
	ctl, err := ReadControl(pool.Store().Path())
	if err != nil {
		return nil, err
	}
	return newCatalog(pool, ctl), nil
}

// Pool returns the buffer pool the catalog reads through.
func (c *Catalog) Pool() *bufmgr.Pool { return c.pool }

// NewOID allocates an object identifier and persists the counter. The
// control file is rewritten before the OID is returned, so OIDs never
// repeat across restarts. Safe for concurrent use.
// PostgreSQL: GetNewObjectId in varsup.c.
func (c *Catalog) NewOID() (tuple.OID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.newOID()
}

// newOID is NewOID with c.mu held.
func (c *Catalog) newOID() (tuple.OID, error) {
	oid := c.control.NextOID
	next := c.control
	next.NextOID++
	if err := WriteControl(c.pool.Store().Path(), next); err != nil {
		return tuple.InvalidOID, err
	}
	c.control = next
	return oid, nil
}

// CreateTable creates the relation file for a new table and records it in
// the catalogs, stamping the catalog rows with xid. Returns
// ErrTooManyColumns for more than MaxColumns columns; ErrDuplicateColumn
// if two columns share a name; ErrExists if name is already in pg_class
// (catalogs included). All three are checked before any OID is allocated
// or any row written. Does not flush.
// PostgreSQL: heap_create_with_catalog in heap.c.
func (c *Catalog) CreateTable(name string, desc *tuple.Desc, xid tuple.XID) (tuple.OID, error) {
	if len(desc.Attrs) > MaxColumns {
		return tuple.InvalidOID, ErrTooManyColumns
	}
	seen := map[string]bool{}
	for _, a := range desc.Attrs {
		if seen[a.Name] {
			return tuple.InvalidOID, ErrDuplicateColumn
		}
		seen[a.Name] = true
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := c.findClass(func(r *RelationInfo) bool { return r.Name == name }); err == nil {
		return tuple.InvalidOID, ErrExists
	} else if !errors.Is(err, ErrNotFound) {
		return tuple.InvalidOID, err
	}

	oid, err := c.newOID()
	if err != nil {
		return tuple.InvalidOID, err
	}
	if _, err := heap.Create(c.pool, oid, desc); err != nil {
		return tuple.InvalidOID, err
	}
	if err := c.insertClass(oid, name, xid); err != nil {
		return tuple.InvalidOID, err
	}
	if err := c.insertAttributes(oid, desc, xid); err != nil {
		return tuple.InvalidOID, err
	}
	c.invalidate()
	return oid, nil
}

// DropTable deletes a table's catalog rows, stamping them with xid, and
// removes its relation file; the table's indexes are dropped with it.
// Returns ErrNotFound for an unknown name; ErrWrongObjectType if name is
// an index; ErrSystemTable for a relation with an OID below FirstUserOID.
// Does not flush.
// PostgreSQL: heap_drop_with_catalog in heap.c.
func (c *Catalog) DropTable(name string, xid tuple.XID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	tid, info, err := c.findClassTID(func(r *RelationInfo) bool { return r.Name == name })
	if err != nil {
		return err
	}
	if info.OID < FirstUserOID {
		return ErrSystemTable
	}
	if err := c.class.Delete(tid, xid); err != nil {
		return err
	}
	attrTIDs, err := c.findAttributeTIDs(info.OID)
	if err != nil {
		return err
	}
	for _, tid := range attrTIDs {
		if err := c.attr.Delete(tid, xid); err != nil {
			return err
		}
	}
	c.invalidate()
	c.pool.Discard(info.OID)
	return c.pool.Store().Unlink(info.OID)
}

// Lookup finds a relation by name. Names are compared exactly. Returns
// ErrNotFound for an unknown or dropped relation. The result is cached: a
// repeated Lookup returns the same pointer and does no I/O. A table's
// Indexes are loaded with it; an index is found too, with an empty Desc.
// PostgreSQL: RelnameGetRelid in namespace.c, then RelationIdGetRelation
// in relcache.c.
func (c *Catalog) Lookup(name string) (*RelationInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if info, ok := c.cache[name]; ok {
		return info, nil
	}
	info, err := c.findClass(func(r *RelationInfo) bool { return r.Name == name })
	if err != nil {
		return nil, err
	}
	if err := c.loadAttributes(info); err != nil {
		return nil, err
	}
	c.cache[name] = info
	return info, nil
}

// LookupOID finds a relation by OID. Returns ErrNotFound for an unknown or
// dropped relation. Shares the cache with Lookup.
// PostgreSQL: RelationIdGetRelation in relcache.c.
func (c *Catalog) LookupOID(oid tuple.OID) (*RelationInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, info := range c.cache {
		if info.OID == oid {
			return info, nil
		}
	}
	info, err := c.findClass(func(r *RelationInfo) bool { return r.OID == oid })
	if err != nil {
		return nil, err
	}
	if err := c.loadAttributes(info); err != nil {
		return nil, err
	}
	c.cache[info.Name] = info
	return info, nil
}

// Tables lists every relation in pg_class, sorted by name, catalogs and
// indexes included. Entries are the cached pointers Lookup returns.
func (c *Catalog) Tables() ([]*RelationInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var infos []*RelationInfo
	err := c.scanClass(func(_ tuple.TID, info *RelationInfo) (bool, error) {
		if cached, ok := c.cache[info.Name]; ok {
			infos = append(infos, cached)
			return true, nil
		}
		if err := c.loadAttributes(info); err != nil {
			return false, err
		}
		c.cache[info.Name] = info
		infos = append(infos, info)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}

// invalidate drops the relation cache. Called after any catalog change.
func (c *Catalog) invalidate() {
	c.cache = map[string]*RelationInfo{}
}

// insertClass adds a pg_class row.
func (c *Catalog) insertClass(oid tuple.OID, name string, xid tuple.XID) error {
	t, err := tuple.Form(ClassDesc, []tuple.Datum{
		int32(oid), name, string(RelKindTable), int32(0), int64(0),
	}, nil)
	if err != nil {
		return err
	}
	_, err = c.class.Insert(t, xid)
	return err
}

// insertAttributes adds one pg_attribute row per column of desc.
func (c *Catalog) insertAttributes(oid tuple.OID, desc *tuple.Desc, xid tuple.XID) error {
	for i, a := range desc.Attrs {
		t, err := tuple.Form(AttributeDesc, []tuple.Datum{
			int32(oid), a.Name, int32(a.Type), int32(i + 1), a.NotNull,
		}, nil)
		if err != nil {
			return err
		}
		if _, err := c.attr.Insert(t, xid); err != nil {
			return err
		}
	}
	return nil
}

// scanClass calls fn for every visible pg_class row until it returns false.
// The RelationInfo has no Desc yet.
func (c *Catalog) scanClass(fn func(tuple.TID, *RelationInfo) (bool, error)) error {
	s := c.class.Scan()
	defer s.Close()
	for s.Next() {
		v, _, err := tuple.Deform(ClassDesc, s.Tuple())
		if err != nil {
			return err
		}
		info := &RelationInfo{
			OID:    tuple.OID(v[0].(int32)),
			Name:   v[1].(string),
			Kind:   RelKind(v[2].(string)[0]),
			Pages:  v[3].(int32),
			Tuples: v[4].(int64),
		}
		more, err := fn(s.TID(), info)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return s.Err()
}

// findClassTID returns the first pg_class row matching pred and its TID.
func (c *Catalog) findClassTID(pred func(*RelationInfo) bool) (tuple.TID, *RelationInfo, error) {
	var (
		foundTID tuple.TID
		found    *RelationInfo
	)
	err := c.scanClass(func(tid tuple.TID, info *RelationInfo) (bool, error) {
		if pred(info) {
			foundTID, found = tid, info
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return tuple.TID{}, nil, err
	}
	if found == nil {
		return tuple.TID{}, nil, ErrNotFound
	}
	return foundTID, found, nil
}

func (c *Catalog) findClass(pred func(*RelationInfo) bool) (*RelationInfo, error) {
	_, info, err := c.findClassTID(pred)
	return info, err
}

// attribute is one pg_attribute row.
type attribute struct {
	tid  tuple.TID
	num  int32
	attr tuple.Attr
}

// scanAttributes returns the pg_attribute rows of a relation sorted by
// attnum.
func (c *Catalog) scanAttributes(oid tuple.OID) ([]attribute, error) {
	s := c.attr.Scan()
	defer s.Close()
	var attrs []attribute
	for s.Next() {
		v, _, err := tuple.Deform(AttributeDesc, s.Tuple())
		if err != nil {
			return nil, err
		}
		if tuple.OID(v[0].(int32)) != oid {
			continue
		}
		attrs = append(attrs, attribute{
			tid: s.TID(),
			num: v[3].(int32),
			attr: tuple.Attr{
				Name:    v[1].(string),
				Type:    tuple.TypeID(v[2].(int32)),
				NotNull: v[4].(bool),
			},
		})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].num < attrs[j].num })
	return attrs, nil
}

// loadAttributes fills info.Desc from pg_attribute.
func (c *Catalog) loadAttributes(info *RelationInfo) error {
	attrs, err := c.scanAttributes(info.OID)
	if err != nil {
		return err
	}
	desc := tuple.NewDesc()
	for _, a := range attrs {
		desc.Attrs = append(desc.Attrs, a.attr)
	}
	info.Desc = desc
	return nil
}

func (c *Catalog) findAttributeTIDs(oid tuple.OID) ([]tuple.TID, error) {
	attrs, err := c.scanAttributes(oid)
	if err != nil {
		return nil, err
	}
	tids := make([]tuple.TID, len(attrs))
	for i, a := range attrs {
		tids[i] = a.tid
	}
	return tids, nil
}

// CreateIndex creates an empty B-tree over column attr (0-based) of the
// table rel, named name, and records it in pg_class (relkind i, no
// pg_attribute rows) and pg_index, stamping the rows with xid. Returns
// ErrNotFound if rel is not in pg_class; ErrWrongObjectType if it is not
// a table; ErrSystemTable if it is a catalog; ErrExists if name is
// already in pg_class. All are checked before any OID is allocated or
// any row written. Does not flush.
// PostgreSQL: index_create in index.c.
func (c *Catalog) CreateIndex(name string, rel tuple.OID, attr int, unique, primary bool, xid tuple.XID) (*IndexInfo, error) {
	panic("not implemented")
}

// DropIndex deletes an index's pg_class and pg_index rows, stamping them
// with xid, and removes its relation file. Returns ErrNotFound for an
// unknown name; ErrWrongObjectType if name is not an index;
// ErrDependentObjects for a primary key index, which only DropTable
// removes. Does not flush.
// PostgreSQL: index_drop in index.c.
func (c *Catalog) DropIndex(name string, xid tuple.XID) error {
	panic("not implemented")
}

// LookupIndex finds an index by name. Returns ErrNotFound for an unknown
// name; ErrWrongObjectType if name is not an index. The result is the
// pointer held in the table's Indexes, so it is shared and read-only.
func (c *Catalog) LookupIndex(name string) (*IndexInfo, error) {
	panic("not implemented")
}
