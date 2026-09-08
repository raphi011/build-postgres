// Package catalog stores the system catalogs pg_class, pg_attribute, and
// pg_index as heap relations and hands out object identifiers. See
// chapters/08-catalog and chapters/13-indexes.
package catalog

import (
	"errors"
	"sync"

	"github.com/raphi011/build-postgres/internal/bufmgr"
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

// Catalog gives access to the system catalogs of one data directory. It is
// safe for concurrent use.
type Catalog struct {
	pool *bufmgr.Pool
	mu   sync.Mutex
}

// Bootstrap initialises an empty data directory: it writes the control
// file, creates pg_class, pg_attribute, and pg_index, and inserts the
// rows that describe them (pg_class rows in that order, then the
// pg_attribute rows of each). Rows are stamped with tuple.BootstrapXID;
// the pool is flushed before returning. Returns ErrBootstrapped if a control file
// exists, ErrControlCorrupt if one exists but is unreadable.
// PostgreSQL: BootstrapModeMain in bootstrap.c.
func Bootstrap(pool *bufmgr.Pool) (*Catalog, error) {
	panic("not implemented")
}

// Open opens the catalogs of a bootstrapped data directory. Reads the
// control file and nothing else. Returns ErrNotBootstrapped if it is
// missing; ErrControlCorrupt if it is damaged.
// PostgreSQL: RelationCacheInitializePhase2 in relcache.c.
func Open(pool *bufmgr.Pool) (*Catalog, error) {
	panic("not implemented")
}

// Pool returns the buffer pool the catalog reads through.
func (c *Catalog) Pool() *bufmgr.Pool { return c.pool }

// NewOID allocates an object identifier and persists the counter. The
// control file is rewritten before the OID is returned, so OIDs never
// repeat across restarts. Safe for concurrent use.
// PostgreSQL: GetNewObjectId in varsup.c.
func (c *Catalog) NewOID() (tuple.OID, error) {
	panic("not implemented")
}

// CreateTable creates the relation file for a new table and records it in
// the catalogs, stamping the catalog rows with xid. Returns
// ErrTooManyColumns for more than MaxColumns columns; ErrDuplicateColumn
// if two columns share a name; ErrExists if name is already in pg_class
// (catalogs included). All three are checked before any OID is allocated
// or any row written. Does not flush.
// PostgreSQL: heap_create_with_catalog in heap.c.
func (c *Catalog) CreateTable(name string, desc *tuple.Desc, xid tuple.XID) (tuple.OID, error) {
	panic("not implemented")
}

// DropTable deletes a table's catalog rows, stamping them with xid, and
// removes its relation file; the table's indexes are dropped with it.
// Returns ErrNotFound for an unknown name; ErrWrongObjectType if name is
// an index; ErrSystemTable for a relation with an OID below FirstUserOID.
// Does not flush.
// PostgreSQL: heap_drop_with_catalog in heap.c.
func (c *Catalog) DropTable(name string, xid tuple.XID) error {
	panic("not implemented")
}

// Lookup finds a relation by name. Names are compared exactly. Returns
// ErrNotFound for an unknown or dropped relation. The result is cached: a
// repeated Lookup returns the same pointer and does no I/O. A table's
// Indexes are loaded with it; an index is found too, with an empty Desc.
// PostgreSQL: RelnameGetRelid in namespace.c, then RelationIdGetRelation
// in relcache.c.
func (c *Catalog) Lookup(name string) (*RelationInfo, error) {
	panic("not implemented")
}

// LookupOID finds a relation by OID. Returns ErrNotFound for an unknown or
// dropped relation. Shares the cache with Lookup.
// PostgreSQL: RelationIdGetRelation in relcache.c.
func (c *Catalog) LookupOID(oid tuple.OID) (*RelationInfo, error) {
	panic("not implemented")
}

// Tables lists every relation in pg_class, sorted by name, catalogs and
// indexes included. Entries are the cached pointers Lookup returns.
func (c *Catalog) Tables() ([]*RelationInfo, error) {
	panic("not implemented")
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
