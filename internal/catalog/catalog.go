// Package catalog stores the system catalogs pg_class and pg_attribute as
// heap relations and hands out object identifiers. See chapters/08-catalog.
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

var (
	ErrBootstrapped    = errors.New("catalog: data directory is already bootstrapped")
	ErrNotBootstrapped = errors.New("catalog: data directory is not bootstrapped")
	ErrControlCorrupt  = errors.New("catalog: control file is corrupt")
	ErrExists          = errors.New("catalog: relation already exists")
	ErrNotFound        = errors.New("catalog: relation does not exist")
	ErrDuplicateColumn = errors.New("catalog: column specified more than once")
	ErrSystemTable     = errors.New("catalog: cannot drop a system catalog")
	ErrTooManyColumns  = errors.New("catalog: too many columns")
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
)

// RelationInfo is a pg_class row together with the relation's columns.
// Values returned by Catalog are shared and must not be modified.
type RelationInfo struct {
	OID    tuple.OID
	Name   string
	Kind   RelKind
	Pages  int32
	Tuples int64
	Desc   *tuple.Desc
}

// Catalog gives access to the system catalogs of one data directory. It is
// safe for concurrent use.
type Catalog struct {
	pool *bufmgr.Pool
	mu   sync.Mutex
}

// Bootstrap initialises an empty data directory: it writes the control
// file, creates pg_class and pg_attribute, and inserts the rows that
// describe them. Rows are stamped with tuple.BootstrapXID; the pool is
// flushed before returning. Returns ErrBootstrapped if a control file
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
// removes its relation file. Returns ErrNotFound for an unknown name;
// ErrSystemTable for a relation with an OID below FirstUserOID. Does not
// flush.
// PostgreSQL: heap_drop_with_catalog in heap.c.
func (c *Catalog) DropTable(name string, xid tuple.XID) error {
	panic("not implemented")
}

// Lookup finds a relation by name. Names are compared exactly. Returns
// ErrNotFound for an unknown or dropped table. The result is cached: a
// repeated Lookup returns the same pointer and does no I/O.
// PostgreSQL: RelnameGetRelid in namespace.c, then RelationIdGetRelation
// in relcache.c.
func (c *Catalog) Lookup(name string) (*RelationInfo, error) {
	panic("not implemented")
}

// LookupOID finds a relation by OID. Returns ErrNotFound for an unknown or
// dropped table. Shares the cache with Lookup.
// PostgreSQL: RelationIdGetRelation in relcache.c.
func (c *Catalog) LookupOID(oid tuple.OID) (*RelationInfo, error) {
	panic("not implemented")
}

// Tables lists every relation in pg_class, sorted by name, catalogs
// included. Entries are the cached pointers Lookup returns.
func (c *Catalog) Tables() ([]*RelationInfo, error) {
	panic("not implemented")
}
