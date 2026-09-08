package catalog

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/raphi011/build-postgres/internal/btree"
	"github.com/raphi011/build-postgres/internal/bufmgr"
	"github.com/raphi011/build-postgres/internal/heap"
	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/tuple"
)

var usersDesc = tuple.NewDesc(
	tuple.Attr{Name: "id", Type: tuple.Int4, NotNull: true},
	tuple.Attr{Name: "name", Type: tuple.Text},
	tuple.Attr{Name: "admin", Type: tuple.Bool},
)

var ordersDesc = tuple.NewDesc(
	tuple.Attr{Name: "id", Type: tuple.Int8, NotNull: true},
	tuple.Attr{Name: "user_id", Type: tuple.Int4},
)

func newPool(t *testing.T, dir string) *bufmgr.Pool {
	t.Helper()
	store, err := smgr.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return bufmgr.New(store, 16)
}

func bootstrap(t *testing.T) (*Catalog, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	pool := newPool(t, dir)
	c, err := Bootstrap(pool)
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

// reopen flushes the pool, drops it, and opens the catalog again over a
// fresh pool, as a restart would.
func reopen(t *testing.T, c *Catalog, dir string) *Catalog {
	t.Helper()
	if err := c.Pool().FlushAll(); err != nil {
		t.Fatal(err)
	}
	if err := c.Pool().Store().Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(newPool(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	return c2
}

// rows scans a relation raw and deforms every visible tuple.
func rows(t *testing.T, pool *bufmgr.Pool, oid tuple.OID, desc *tuple.Desc) [][]tuple.Datum {
	t.Helper()
	rel := heap.Open(pool, oid, desc)
	s := rel.Scan(nil)
	defer s.Close()
	var out [][]tuple.Datum
	for s.Next() {
		values, nulls, err := tuple.Deform(desc, s.Tuple())
		if err != nil {
			t.Fatal(err)
		}
		for i, n := range nulls {
			if n {
				t.Fatalf("catalog row has NULL in column %d", i)
			}
		}
		out = append(out, values)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func classRow(oid tuple.OID, name string) []tuple.Datum {
	return []tuple.Datum{int32(oid), name, "r", int32(0), int64(0)}
}

func indexClassRow(oid tuple.OID, name string) []tuple.Datum {
	return []tuple.Datum{int32(oid), name, "i", int32(0), int64(0)}
}

func indexRow(idx *IndexInfo) []tuple.Datum {
	return []tuple.Datum{int32(idx.OID), int32(idx.Rel), int32(idx.Attr + 1), idx.Unique, idx.Primary}
}

// catalogClassRows are the pg_class rows Bootstrap writes, in order.
var catalogClassRows = [][]tuple.Datum{
	classRow(ClassOID, "pg_class"),
	classRow(AttributeOID, "pg_attribute"),
	classRow(IndexOID, "pg_index"),
}

// catalogAttrRows are the pg_attribute rows Bootstrap writes, in order.
func catalogAttrRows() [][]tuple.Datum {
	rows := attrRows(ClassOID, ClassDesc)
	rows = append(rows, attrRows(AttributeOID, AttributeDesc)...)
	return append(rows, attrRows(IndexOID, IndexDesc)...)
}

func attrRows(oid tuple.OID, desc *tuple.Desc) [][]tuple.Datum {
	var out [][]tuple.Datum
	for i, a := range desc.Attrs {
		out = append(out, []tuple.Datum{int32(oid), a.Name, int32(a.Type), int32(i + 1), a.NotNull})
	}
	return out
}

func names(infos []*RelationInfo) []string {
	var out []string
	for _, info := range infos {
		out = append(out, info.Name)
	}
	return out
}

// Control file.

var goldenControl = []byte{
	'P', 'G', 'D', 'B',
	0x01, 0x00, 0x00, 0x00, // version 1
	0x00, 0x40, 0x00, 0x00, // next OID 16384
	0x03, 0x00, 0x00, 0x00, // next XID 3
	0xa4, 0x3d, 0xc7, 0x3d, // CRC-32 of the 16 bytes above
}

func TestControlGolden(t *testing.T) {
	_, dir := bootstrap(t)
	got, err := os.ReadFile(filepath.Join(dir, "global", "control"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, goldenControl) {
		t.Fatalf("control file\n got % x\nwant % x", got, goldenControl)
	}
}

func TestControlRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadControl(dir); !errors.Is(err, ErrNotBootstrapped) {
		t.Fatalf("ReadControl on empty dir: %v, want ErrNotBootstrapped", err)
	}
	want := Control{NextOID: 70000, NextXID: 12345}
	if err := WriteControl(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ReadControl = %+v, want %+v", got, want)
	}
	// Overwrite and read again.
	want.NextOID++
	if err := WriteControl(dir, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadControl(dir); got != want {
		t.Fatalf("after rewrite = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "global", "control.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control.tmp left behind: %v", err)
	}
}

func TestControlCorrupt(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(b []byte) []byte
	}{
		{"bad magic", func(b []byte) []byte { b[0] = 'X'; return b }},
		{"bad version", func(b []byte) []byte { b[4] = 2; return b }},
		{"bad crc", func(b []byte) []byte { b[16] ^= 0xff; return b }},
		{"flipped payload", func(b []byte) []byte { b[9] ^= 0x01; return b }},
		{"truncated", func(b []byte) []byte { return b[:16] }},
		{"trailing bytes", func(b []byte) []byte { return append(b, 0) }},
		{"empty", func(b []byte) []byte { return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := WriteControl(dir, Control{NextOID: FirstUserOID, NextXID: tuple.FirstNormalXID}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "global", "control")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.mutate(b), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadControl(dir); !errors.Is(err, ErrControlCorrupt) {
				t.Fatalf("ReadControl = %v, want ErrControlCorrupt", err)
			}
			if _, err := Open(newPool(t, dir)); !errors.Is(err, ErrControlCorrupt) {
				t.Fatalf("Open = %v, want ErrControlCorrupt", err)
			}
		})
	}
}

// Bootstrap.

func TestBootstrapDescribesItself(t *testing.T) {
	c, _ := bootstrap(t)
	pool := c.Pool()

	if got := rows(t, pool, ClassOID, ClassDesc); !reflect.DeepEqual(got, catalogClassRows) {
		t.Fatalf("pg_class rows\n got %v\nwant %v", got, catalogClassRows)
	}
	if got, want := rows(t, pool, AttributeOID, AttributeDesc), catalogAttrRows(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pg_attribute rows\n got %v\nwant %v", got, want)
	}
	if got := rows(t, pool, IndexOID, IndexDesc); len(got) != 0 {
		t.Fatalf("pg_index rows after bootstrap: %v", got)
	}

	// Rows are stamped with the bootstrap XID.
	s := heap.Open(pool, ClassOID, ClassDesc).Scan(nil)
	defer s.Close()
	for s.Next() {
		if s.Tuple().Xmin() != tuple.BootstrapXID {
			t.Fatalf("xmin = %d, want BootstrapXID", s.Tuple().Xmin())
		}
	}

	// And the catalog can read its own description back.
	for _, tc := range []struct {
		name string
		oid  tuple.OID
		desc *tuple.Desc
	}{{"pg_class", ClassOID, ClassDesc}, {"pg_attribute", AttributeOID, AttributeDesc}, {"pg_index", IndexOID, IndexDesc}} {
		info, err := c.Lookup(tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if info.OID != tc.oid || info.Name != tc.name || info.Kind != RelKindTable || info.Indexes != nil {
			t.Fatalf("Lookup(%s) = %+v", tc.name, info)
		}
		if !reflect.DeepEqual(info.Desc, tc.desc) {
			t.Fatalf("Lookup(%s).Desc = %+v, want %+v", tc.name, info.Desc, tc.desc)
		}
	}
}

func TestBootstrapIsDurable(t *testing.T) {
	c, dir := bootstrap(t)
	// Bootstrap flushes: a fresh pool without FlushAll sees the rows.
	if err := c.Pool().Store().Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(newPool(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := rows(t, c2.Pool(), ClassOID, ClassDesc); len(got) != 3 {
		t.Fatalf("pg_class has %d rows after reopen, want 3", len(got))
	}
}

func TestBootstrapTwice(t *testing.T) {
	c, _ := bootstrap(t)
	if _, err := Bootstrap(c.Pool()); !errors.Is(err, ErrBootstrapped) {
		t.Fatalf("second Bootstrap = %v, want ErrBootstrapped", err)
	}
}

func TestOpenNotBootstrapped(t *testing.T) {
	pool := newPool(t, filepath.Join(t.TempDir(), "data"))
	if _, err := Open(pool); !errors.Is(err, ErrNotBootstrapped) {
		t.Fatalf("Open = %v, want ErrNotBootstrapped", err)
	}
}

// Tables.

func TestCreateTable(t *testing.T) {
	c, _ := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	if oid != FirstUserOID {
		t.Fatalf("first user OID = %d, want %d", oid, FirstUserOID)
	}
	oid2, err := c.CreateTable("orders", ordersDesc, 11)
	if err != nil {
		t.Fatal(err)
	}
	if oid2 != FirstUserOID+1 {
		t.Fatalf("second user OID = %d, want %d", oid2, FirstUserOID+1)
	}

	info, err := c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	want := &RelationInfo{OID: oid, Name: "users", Kind: RelKindTable, Desc: usersDesc}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("Lookup = %+v, want %+v", info, want)
	}
	byOID, err := c.LookupOID(oid2)
	if err != nil {
		t.Fatal(err)
	}
	if byOID.Name != "orders" || !reflect.DeepEqual(byOID.Desc, ordersDesc) {
		t.Fatalf("LookupOID = %+v", byOID)
	}

	exists, err := c.Pool().Store().Exists(oid)
	if err != nil || !exists {
		t.Fatalf("relation file for %d: exists=%v err=%v", oid, exists, err)
	}

	// Catalog rows in the documented order, stamped with the caller's XID.
	wantClass := append(catalogClassRows, classRow(oid, "users"), classRow(oid2, "orders"))
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); !reflect.DeepEqual(got, wantClass) {
		t.Fatalf("pg_class rows\n got %v\nwant %v", got, wantClass)
	}
	wantAttr := catalogAttrRows()
	wantAttr = append(wantAttr, attrRows(oid, usersDesc)...)
	wantAttr = append(wantAttr, attrRows(oid2, ordersDesc)...)
	if got := rows(t, c.Pool(), AttributeOID, AttributeDesc); !reflect.DeepEqual(got, wantAttr) {
		t.Fatalf("pg_attribute rows\n got %v\nwant %v", got, wantAttr)
	}
	s := heap.Open(c.Pool(), ClassOID, ClassDesc).Scan(nil)
	defer s.Close()
	var xmins []tuple.XID
	for s.Next() {
		xmins = append(xmins, s.Tuple().Xmin())
	}
	if want := []tuple.XID{1, 1, 1, 10, 11}; !reflect.DeepEqual(xmins, want) {
		t.Fatalf("pg_class xmins = %v, want %v", xmins, want)
	}
}

func TestCreateTableNoColumns(t *testing.T) {
	c, _ := bootstrap(t)
	if _, err := c.CreateTable("empty", tuple.NewDesc(), 10); err != nil {
		t.Fatal(err)
	}
	info, err := c.Lookup("empty")
	if err != nil {
		t.Fatal(err)
	}
	if info.Desc.Len() != 0 {
		t.Fatalf("Desc has %d columns, want 0", info.Desc.Len())
	}
}

// wideDesc builds a descriptor with n bool columns.
func wideDesc(n int) *tuple.Desc {
	attrs := make([]tuple.Attr, n)
	for i := range attrs {
		attrs[i] = tuple.Attr{Name: fmt.Sprintf("c%d", i), Type: tuple.Bool}
	}
	return tuple.NewDesc(attrs...)
}

func TestCreateTableErrors(t *testing.T) {
	c, _ := bootstrap(t)
	if _, err := c.CreateTable("users", usersDesc, 10); err != nil {
		t.Fatal(err)
	}
	before := rows(t, c.Pool(), AttributeOID, AttributeDesc)

	cases := []struct {
		name string
		tbl  string
		desc *tuple.Desc
		want error
	}{
		{"duplicate name", "users", ordersDesc, ErrExists},
		{"catalog name", "pg_class", ordersDesc, ErrExists},
		{"duplicate column", "t", tuple.NewDesc(
			tuple.Attr{Name: "a", Type: tuple.Int4},
			tuple.Attr{Name: "b", Type: tuple.Text},
			tuple.Attr{Name: "a", Type: tuple.Bool},
		), ErrDuplicateColumn},
		{"too many columns", "t", wideDesc(MaxColumns + 1), ErrTooManyColumns},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.CreateTable(tc.tbl, tc.desc, 10); !errors.Is(err, tc.want) {
				t.Fatalf("CreateTable = %v, want %v", err, tc.want)
			}
		})
	}

	// Nothing was allocated or written by the failed calls.
	if after := rows(t, c.Pool(), AttributeOID, AttributeDesc); !reflect.DeepEqual(after, before) {
		t.Fatal("failed CreateTable wrote pg_attribute rows")
	}
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); len(got) != 4 {
		t.Fatalf("pg_class has %d rows, want 4", len(got))
	}
	oid, err := c.NewOID()
	if err != nil {
		t.Fatal(err)
	}
	if oid != FirstUserOID+1 {
		t.Fatalf("NewOID after failed creates = %d, want %d", oid, FirstUserOID+1)
	}
	if _, err := c.Lookup("t"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup(t) = %v, want ErrNotFound", err)
	}
}

func TestLookupNotFound(t *testing.T) {
	c, _ := bootstrap(t)
	if _, err := c.Lookup("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup = %v, want ErrNotFound", err)
	}
	if _, err := c.LookupOID(FirstUserOID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupOID = %v, want ErrNotFound", err)
	}
	// Names are compared exactly; the parser folds case, not the catalog.
	if _, err := c.Lookup("PG_CLASS"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup(PG_CLASS) = %v, want ErrNotFound", err)
	}
}

func TestTablesSorted(t *testing.T) {
	c, _ := bootstrap(t)
	for _, name := range []string{"zebra", "apple", "mango"} {
		if _, err := c.CreateTable(name, usersDesc, 10); err != nil {
			t.Fatal(err)
		}
	}
	infos, err := c.Tables()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"apple", "mango", "pg_attribute", "pg_class", "pg_index", "zebra"}
	if got := names(infos); !reflect.DeepEqual(got, want) {
		t.Fatalf("Tables = %v, want %v", got, want)
	}
	for _, info := range infos {
		if info.Desc == nil || info.Kind != RelKindTable {
			t.Fatalf("Tables entry incomplete: %+v", info)
		}
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	c, dir := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	c = reopen(t, c, dir)

	info, err := c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	if info.OID != oid || !reflect.DeepEqual(info.Desc, usersDesc) {
		t.Fatalf("after reopen Lookup = %+v", info)
	}
	infos, err := c.Tables()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(infos); !reflect.DeepEqual(got, []string{"pg_attribute", "pg_class", "pg_index", "users"}) {
		t.Fatalf("Tables after reopen = %v", got)
	}
}

func TestCreatedTableIsUsable(t *testing.T) {
	c, dir := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	rel := heap.Open(c.Pool(), oid, usersDesc)
	tup, err := tuple.Form(usersDesc, []tuple.Datum{int32(1), "ann", true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rel.Insert(tup, 10); err != nil {
		t.Fatal(err)
	}

	c = reopen(t, c, dir)
	info, err := c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	got := rows(t, c.Pool(), info.OID, info.Desc)
	want := [][]tuple.Datum{{int32(1), "ann", true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

// Drop.

func TestDropTable(t *testing.T) {
	c, dir := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTable("orders", ordersDesc, 10); err != nil {
		t.Fatal(err)
	}
	if err := c.DropTable("users", 20); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Lookup("users"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup after drop = %v, want ErrNotFound", err)
	}
	if _, err := c.LookupOID(oid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupOID after drop = %v, want ErrNotFound", err)
	}
	// The file goes when the transaction commits.
	if exists, err := c.Pool().Store().Exists(oid); err != nil || !exists {
		t.Fatalf("relation file before commit: exists=%v err=%v", exists, err)
	}
	if err := c.EndTransaction(true); err != nil {
		t.Fatal(err)
	}
	if exists, err := c.Pool().Store().Exists(oid); err != nil || exists {
		t.Fatalf("relation file after drop: exists=%v err=%v", exists, err)
	}
	wantClass := append(catalogClassRows, classRow(oid+1, "orders"))
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); !reflect.DeepEqual(got, wantClass) {
		t.Fatalf("pg_class rows after drop\n got %v\nwant %v", got, wantClass)
	}
	wantAttr := catalogAttrRows()
	wantAttr = append(wantAttr, attrRows(oid+1, ordersDesc)...)
	if got := rows(t, c.Pool(), AttributeOID, AttributeDesc); !reflect.DeepEqual(got, wantAttr) {
		t.Fatalf("pg_attribute rows after drop\n got %v\nwant %v", got, wantAttr)
	}

	// The deleted rows are still there, stamped with the dropping XID.
	rel := heap.Open(c.Pool(), ClassOID, ClassDesc)
	old, _, err := rel.Fetch(tuple.TID{Block: 0, Off: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if old.Xmax() != 20 {
		t.Fatalf("dropped pg_class row xmax = %d, want 20", old.Xmax())
	}

	// The name is free again and the new table gets a fresh OID.
	oid2, err := c.CreateTable("users", ordersDesc, 30)
	if err != nil {
		t.Fatal(err)
	}
	if oid2 <= oid+1 {
		t.Fatalf("recreated table OID = %d, want > %d", oid2, oid+1)
	}
	info, err := c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	if info.OID != oid2 || !reflect.DeepEqual(info.Desc, ordersDesc) {
		t.Fatalf("recreated Lookup = %+v", info)
	}

	c = reopen(t, c, dir)
	infos, err := c.Tables()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(infos); !reflect.DeepEqual(got, []string{"orders", "pg_attribute", "pg_class", "pg_index", "users"}) {
		t.Fatalf("Tables after reopen = %v", got)
	}
}

func TestDropTableErrors(t *testing.T) {
	c, _ := bootstrap(t)
	if err := c.DropTable("nope", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DropTable(nope) = %v, want ErrNotFound", err)
	}
	for _, name := range []string{"pg_class", "pg_attribute", "pg_index"} {
		if err := c.DropTable(name, 10); !errors.Is(err, ErrSystemTable) {
			t.Fatalf("DropTable(%s) = %v, want ErrSystemTable", name, err)
		}
		if _, err := c.Lookup(name); err != nil {
			t.Fatalf("%s gone after refused drop: %v", name, err)
		}
	}
}

// OIDs.

func TestOIDsNeverRepeat(t *testing.T) {
	c, dir := bootstrap(t)
	seen := map[tuple.OID]bool{}
	var last tuple.OID
	take := func(c *Catalog, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			oid, err := c.NewOID()
			if err != nil {
				t.Fatal(err)
			}
			if oid <= last || seen[oid] {
				t.Fatalf("OID %d after %d", oid, last)
			}
			seen[oid] = true
			last = oid
		}
	}
	take(c, 3)
	if _, err := c.CreateTable("t", usersDesc, 10); err != nil {
		t.Fatal(err)
	}
	seen[FirstUserOID+3] = true
	last = FirstUserOID + 3
	for i := 0; i < 3; i++ {
		c = reopen(t, c, dir)
		take(c, 5)
	}
	ctl, err := ReadControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ctl.NextOID != last+1 {
		t.Fatalf("control NextOID = %d, want %d", ctl.NextOID, last+1)
	}
	if ctl.NextXID != tuple.FirstNormalXID {
		t.Fatalf("control NextXID = %d, want %d", ctl.NextXID, tuple.FirstNormalXID)
	}
}

func TestNewOIDConcurrent(t *testing.T) {
	c, dir := bootstrap(t)
	const workers, each = 8, 25
	var mu sync.Mutex
	var got []tuple.OID
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				oid, err := c.NewOID()
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				got = append(got, oid)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	if len(got) != workers*each {
		t.Fatalf("got %d OIDs", len(got))
	}
	for i, oid := range got {
		if oid != FirstUserOID+tuple.OID(i) {
			t.Fatalf("OIDs not dense and unique: %v", got)
		}
	}
	ctl, err := ReadControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ctl.NextOID != FirstUserOID+workers*each {
		t.Fatalf("control NextOID = %d, want %d", ctl.NextOID, FirstUserOID+workers*each)
	}
}

// Cache.

func TestLookupCache(t *testing.T) {
	c, _ := bootstrap(t)
	if _, err := c.CreateTable("users", usersDesc, 10); err != nil {
		t.Fatal(err)
	}
	first, err := c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	before := c.Pool().Stats()
	second, err := c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	if after := c.Pool().Stats(); after != before {
		t.Fatalf("second Lookup touched the pool: %+v -> %+v", before, after)
	}
	if first != second {
		t.Fatal("second Lookup returned a different RelationInfo")
	}

	// DDL invalidates: a drop is seen, and a recreate is seen with the new
	// descriptor.
	if err := c.DropTable("users", 11); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lookup("users"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup after drop = %v, want ErrNotFound", err)
	}
	if _, err := c.CreateTable("users", ordersDesc, 12); err != nil {
		t.Fatal(err)
	}
	info, err := c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(info.Desc, ordersDesc) {
		t.Fatalf("Lookup after recreate has stale Desc %+v", info.Desc)
	}
}

func TestLookupConcurrent(t *testing.T) {
	c, _ := bootstrap(t)
	if _, err := c.CreateTable("users", usersDesc, 10); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				info, err := c.Lookup("users")
				if err != nil {
					t.Error(err)
					return
				}
				if info.Name != "users" {
					t.Errorf("Lookup = %+v", info)
					return
				}
				if _, err := c.Tables(); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Indexes.

// createIndex creates an index and fails the test on error.
func createIndex(t *testing.T, c *Catalog, name string, rel tuple.OID, attr int, unique, primary bool, xid tuple.XID) *IndexInfo {
	t.Helper()
	idx, err := c.CreateIndex(name, rel, attr, unique, primary, xid)
	if err != nil {
		t.Fatalf("CreateIndex(%s): %v", name, err)
	}
	return idx
}

func indexNames(idxs []*IndexInfo) []string {
	var out []string
	for _, idx := range idxs {
		out = append(out, idx.Name)
	}
	return out
}

func TestCreateIndex(t *testing.T) {
	c, dir := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	idx := createIndex(t, c, "users_pkey", oid, 0, true, true, 11)
	want := &IndexInfo{OID: oid + 1, Name: "users_pkey", Rel: oid, Attr: 0, Unique: true, Primary: true}
	if !reflect.DeepEqual(idx, want) {
		t.Fatalf("CreateIndex = %+v, want %+v", idx, want)
	}

	// Catalog rows: pg_class gets a relkind i row, pg_index the index row,
	// pg_attribute nothing; all stamped with the caller's XID.
	wantClass := append(catalogClassRows, classRow(oid, "users"), indexClassRow(idx.OID, "users_pkey"))
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); !reflect.DeepEqual(got, wantClass) {
		t.Fatalf("pg_class rows\n got %v\nwant %v", got, wantClass)
	}
	if got, want := rows(t, c.Pool(), IndexOID, IndexDesc), [][]tuple.Datum{indexRow(idx)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pg_index rows\n got %v\nwant %v", got, want)
	}
	if got, want := rows(t, c.Pool(), AttributeOID, AttributeDesc), append(catalogAttrRows(), attrRows(oid, usersDesc)...); !reflect.DeepEqual(got, want) {
		t.Fatalf("pg_attribute rows\n got %v\nwant %v", got, want)
	}
	s := heap.Open(c.Pool(), IndexOID, IndexDesc).Scan(nil)
	defer s.Close()
	for s.Next() {
		if s.Tuple().Xmin() != 11 {
			t.Fatalf("pg_index row xmin = %d, want 11", s.Tuple().Xmin())
		}
	}

	// The index file is an empty tree keyed by the column's type.
	tree := btree.Open(c.Pool(), idx.OID, tuple.Int4)
	meta, err := tree.Meta()
	if err != nil || meta.Root != btree.None {
		t.Fatalf("index file: meta %+v, err %v", meta, err)
	}

	// The table knows its index; the index is a relation of kind i with
	// no columns; LookupIndex returns the table's pointer.
	info, err := c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Indexes) != 1 || !reflect.DeepEqual(info.Indexes[0], want) {
		t.Fatalf("Lookup(users).Indexes = %+v", info.Indexes)
	}
	byName, err := c.LookupIndex("users_pkey")
	if err != nil {
		t.Fatal(err)
	}
	if byName != info.Indexes[0] {
		t.Error("LookupIndex returned a different IndexInfo than Lookup(users).Indexes")
	}
	rel, err := c.Lookup("users_pkey")
	if err != nil {
		t.Fatal(err)
	}
	if rel.OID != idx.OID || rel.Kind != RelKindIndex || rel.Desc.Len() != 0 || rel.Indexes != nil {
		t.Fatalf("Lookup(users_pkey) = %+v", rel)
	}
	if byOID, err := c.LookupOID(idx.OID); err != nil || byOID != rel {
		t.Fatalf("LookupOID(index) = %+v, %v", byOID, err)
	}

	// Everything survives a restart.
	c = reopen(t, c, dir)
	info, err = c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Indexes) != 1 || !reflect.DeepEqual(info.Indexes[0], want) {
		t.Fatalf("after reopen Lookup(users).Indexes = %+v", info.Indexes)
	}
	infos, err := c.Tables()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(infos); !reflect.DeepEqual(got, []string{"pg_attribute", "pg_class", "pg_index", "users", "users_pkey"}) {
		t.Fatalf("Tables = %v", got)
	}
}

func TestIndexesOrder(t *testing.T) {
	c, _ := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	createIndex(t, c, "users_name", oid, 1, false, false, 10)
	createIndex(t, c, "users_admin", oid, 2, false, false, 10)
	createIndex(t, c, "users_pkey", oid, 0, true, true, 10)
	info, err := c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	// The primary key first, then by name, whatever the creation order.
	want := []string{"users_pkey", "users_admin", "users_name"}
	if got := indexNames(info.Indexes); !reflect.DeepEqual(got, want) {
		t.Fatalf("Indexes = %v, want %v", got, want)
	}
	for _, idx := range info.Indexes {
		if idx.Rel != oid || idx.Attr < 0 || idx.Attr > 2 {
			t.Errorf("index %+v", idx)
		}
	}
	// Another table's indexes are not mixed in.
	oid2, err := c.CreateTable("orders", ordersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	createIndex(t, c, "orders_pkey", oid2, 0, true, true, 10)
	info, _ = c.Lookup("users")
	if got := indexNames(info.Indexes); !reflect.DeepEqual(got, want) {
		t.Fatalf("Indexes after another table's index = %v", got)
	}
	orders, _ := c.Lookup("orders")
	if got := indexNames(orders.Indexes); !reflect.DeepEqual(got, []string{"orders_pkey"}) {
		t.Fatalf("orders Indexes = %v", got)
	}
}

func TestCreateIndexErrors(t *testing.T) {
	c, _ := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	idx := createIndex(t, c, "users_name", oid, 1, false, false, 10)
	before := rows(t, c.Pool(), ClassOID, ClassDesc)

	cases := []struct {
		name string
		idx  string
		rel  tuple.OID
		want error
	}{
		{"table name", "users", oid, ErrExists},
		{"index name", "users_name", oid, ErrExists},
		{"catalog name", "pg_index", oid, ErrExists},
		{"unknown table", "x", oid + 50, ErrNotFound},
		{"table is an index", "x", idx.OID, ErrWrongObjectType},
		{"catalog table", "x", ClassOID, ErrSystemTable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.CreateIndex(tc.idx, tc.rel, 0, false, false, 10); !errors.Is(err, tc.want) {
				t.Fatalf("CreateIndex = %v, want %v", err, tc.want)
			}
		})
	}

	// Nothing was allocated or written by the failed calls.
	if after := rows(t, c.Pool(), ClassOID, ClassDesc); !reflect.DeepEqual(after, before) {
		t.Fatal("failed CreateIndex wrote pg_class rows")
	}
	if got := rows(t, c.Pool(), IndexOID, IndexDesc); len(got) != 1 {
		t.Fatalf("pg_index has %d rows, want 1", len(got))
	}
	next, err := c.NewOID()
	if err != nil {
		t.Fatal(err)
	}
	if next != idx.OID+1 {
		t.Fatalf("NewOID after failed creates = %d, want %d", next, idx.OID+1)
	}
	if _, err := c.LookupIndex("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupIndex(x) = %v, want ErrNotFound", err)
	}
	if _, err := c.LookupIndex("users"); !errors.Is(err, ErrWrongObjectType) {
		t.Fatalf("LookupIndex(users) = %v, want ErrWrongObjectType", err)
	}
}

func TestDropIndex(t *testing.T) {
	c, dir := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	pkey := createIndex(t, c, "users_pkey", oid, 0, true, true, 10)
	name := createIndex(t, c, "users_name", oid, 1, false, false, 10)

	if err := c.DropIndex("users_name", 20); err != nil {
		t.Fatal(err)
	}
	if _, err := c.LookupIndex("users_name"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupIndex after drop = %v, want ErrNotFound", err)
	}
	if _, err := c.Lookup("users_name"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup after drop = %v, want ErrNotFound", err)
	}
	if err := c.EndTransaction(true); err != nil {
		t.Fatal(err)
	}
	if exists, err := c.Pool().Store().Exists(name.OID); err != nil || exists {
		t.Fatalf("index file after drop: exists=%v err=%v", exists, err)
	}
	info, _ := c.Lookup("users")
	if got := indexNames(info.Indexes); !reflect.DeepEqual(got, []string{"users_pkey"}) {
		t.Fatalf("Indexes after drop = %v", got)
	}
	wantClass := append(catalogClassRows, classRow(oid, "users"), indexClassRow(pkey.OID, "users_pkey"))
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); !reflect.DeepEqual(got, wantClass) {
		t.Fatalf("pg_class rows after drop\n got %v\nwant %v", got, wantClass)
	}
	if got, want := rows(t, c.Pool(), IndexOID, IndexDesc), [][]tuple.Datum{indexRow(pkey)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pg_index rows after drop\n got %v\nwant %v", got, want)
	}
	// The deleted rows are still there, stamped with the dropping XID.
	for _, r := range []struct {
		oid  tuple.OID
		desc *tuple.Desc
		tid  tuple.TID
	}{{ClassOID, ClassDesc, tuple.TID{Block: 0, Off: 6}}, {IndexOID, IndexDesc, tuple.TID{Block: 0, Off: 2}}} {
		old, _, err := heap.Open(c.Pool(), r.oid, r.desc).Fetch(r.tid, nil)
		if err != nil {
			t.Fatal(err)
		}
		if old.Xmax() != 20 {
			t.Fatalf("dropped row in %d has xmax %d, want 20", r.oid, old.Xmax())
		}
	}

	// Errors: unknown, a table, a primary key; nothing changes.
	if err := c.DropIndex("nope", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DropIndex(nope) = %v, want ErrNotFound", err)
	}
	if err := c.DropIndex("users", 10); !errors.Is(err, ErrWrongObjectType) {
		t.Fatalf("DropIndex(users) = %v, want ErrWrongObjectType", err)
	}
	if err := c.DropIndex("pg_class", 10); !errors.Is(err, ErrWrongObjectType) {
		t.Fatalf("DropIndex(pg_class) = %v, want ErrWrongObjectType", err)
	}
	if err := c.DropIndex("users_pkey", 10); !errors.Is(err, ErrDependentObjects) {
		t.Fatalf("DropIndex(users_pkey) = %v, want ErrDependentObjects", err)
	}
	if _, err := c.Lookup("users"); err != nil {
		t.Fatalf("users gone after refused drop: %v", err)
	}
	if _, err := c.LookupIndex("users_pkey"); err != nil {
		t.Fatalf("users_pkey gone after refused drop: %v", err)
	}

	// The name is free again; the drop survives a restart.
	createIndex(t, c, "users_name", oid, 2, false, false, 30)
	c = reopen(t, c, dir)
	info, err = c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	if got := indexNames(info.Indexes); !reflect.DeepEqual(got, []string{"users_pkey", "users_name"}) {
		t.Fatalf("Indexes after reopen = %v", got)
	}
	if info.Indexes[1].Attr != 2 || info.Indexes[1].OID <= name.OID {
		t.Fatalf("recreated index = %+v", info.Indexes[1])
	}
}

func TestDropTableDropsIndexes(t *testing.T) {
	c, _ := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	pkey := createIndex(t, c, "users_pkey", oid, 0, true, true, 10)
	name := createIndex(t, c, "users_name", oid, 1, false, false, 10)
	oid2, err := c.CreateTable("orders", ordersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	other := createIndex(t, c, "orders_pkey", oid2, 0, true, true, 10)

	if err := c.DropTable("users_pkey", 10); !errors.Is(err, ErrWrongObjectType) {
		t.Fatalf("DropTable(index) = %v, want ErrWrongObjectType", err)
	}
	if err := c.DropTable("users", 20); err != nil {
		t.Fatal(err)
	}
	if err := c.EndTransaction(true); err != nil {
		t.Fatal(err)
	}
	for _, idx := range []*IndexInfo{pkey, name} {
		if _, err := c.LookupIndex(idx.Name); !errors.Is(err, ErrNotFound) {
			t.Errorf("LookupIndex(%s) after DropTable = %v, want ErrNotFound", idx.Name, err)
		}
		if exists, err := c.Pool().Store().Exists(idx.OID); err != nil || exists {
			t.Errorf("index file %d after DropTable: exists=%v err=%v", idx.OID, exists, err)
		}
	}
	wantClass := append(catalogClassRows, classRow(oid2, "orders"), indexClassRow(other.OID, "orders_pkey"))
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); !reflect.DeepEqual(got, wantClass) {
		t.Fatalf("pg_class rows after drop\n got %v\nwant %v", got, wantClass)
	}
	if got, want := rows(t, c.Pool(), IndexOID, IndexDesc), [][]tuple.Datum{indexRow(other)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pg_index rows after drop\n got %v\nwant %v", got, want)
	}
	infos, err := c.Tables()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(infos); !reflect.DeepEqual(got, []string{"orders", "orders_pkey", "pg_attribute", "pg_class", "pg_index"}) {
		t.Fatalf("Tables = %v", got)
	}
}

func TestIndexCache(t *testing.T) {
	c, _ := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := c.Lookup("users")
	if len(first.Indexes) != 0 {
		t.Fatalf("Indexes of a new table = %v", first.Indexes)
	}
	createIndex(t, c, "users_pkey", oid, 0, true, true, 10)
	second, _ := c.Lookup("users")
	if second == first || len(second.Indexes) != 1 {
		t.Fatalf("Lookup after CreateIndex: same pointer %v, Indexes %v", second == first, second.Indexes)
	}
	idx, err := c.LookupIndex("users_pkey")
	if err != nil {
		t.Fatal(err)
	}
	if idx != second.Indexes[0] {
		t.Error("LookupIndex returned a different pointer than the cached table")
	}
	if err := c.DropIndex("users_pkey", 10); err == nil {
		t.Fatal("dropped a primary key")
	}
	createIndex(t, c, "users_name", oid, 1, false, false, 10)
	if err := c.DropIndex("users_name", 10); err != nil {
		t.Fatal(err)
	}
	third, _ := c.Lookup("users")
	if got := indexNames(third.Indexes); !reflect.DeepEqual(got, []string{"users_pkey"}) {
		t.Fatalf("Indexes after DropIndex = %v", got)
	}
}

// Statistics (chapter 14).

func TestUpdateStats(t *testing.T) {
	c, dir := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 10)
	if err != nil {
		t.Fatal(err)
	}
	idx := createIndex(t, c, "users_pkey", oid, 0, true, true, 10)
	if _, err := c.CreateTable("orders", ordersDesc, 10); err != nil {
		t.Fatal(err)
	}
	info, _ := c.Lookup("users")
	if info.Pages != 0 || info.Tuples != 0 || info.Indexes[0].Pages != 0 || info.Indexes[0].Tuples != 0 {
		t.Fatalf("fresh statistics: %+v %+v", info, info.Indexes[0])
	}

	if err := c.UpdateStats(oid, 3, 250, 20); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateStats(idx.OID, 2, 250, 20); err != nil {
		t.Fatal(err)
	}
	// The cache was invalidated and the new values are visible through
	// every lookup path.
	info, _ = c.Lookup("users")
	if info.Pages != 3 || info.Tuples != 250 {
		t.Errorf("Lookup after UpdateStats: pages %d tuples %d", info.Pages, info.Tuples)
	}
	if got := info.Indexes[0]; got.Pages != 2 || got.Tuples != 250 {
		t.Errorf("index statistics: %+v", got)
	}
	if byIdx, _ := c.LookupIndex("users_pkey"); byIdx.Pages != 2 {
		t.Errorf("LookupIndex statistics: %+v", byIdx)
	}
	if rel, _ := c.LookupOID(idx.OID); rel.Pages != 2 || rel.Tuples != 250 {
		t.Errorf("index relation statistics: %+v", rel)
	}

	// The pg_class row is updated in place (a new version at the end,
	// the old one stamped with xid); nothing else changes.
	want := [][]tuple.Datum{
		classRow(ClassOID, "pg_class"),
		classRow(AttributeOID, "pg_attribute"),
		classRow(IndexOID, "pg_index"),
		classRow(oid+2, "orders"),
		{int32(oid), "users", "r", int32(3), int64(250)},
		{int32(idx.OID), "users_pkey", "i", int32(2), int64(250)},
	}
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); !reflect.DeepEqual(got, want) {
		t.Errorf("pg_class rows\n got %v\nwant %v", got, want)
	}
	old, _, err := heap.Open(c.Pool(), ClassOID, ClassDesc).Fetch(tuple.TID{Block: 0, Off: 4}, nil)
	if err != nil || old.Xmax() != 20 {
		t.Errorf("old users row: xmax %d, err %v", old.Xmax(), err)
	}
	if err := c.UpdateStats(oid+50, 1, 1, 20); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateStats(unknown) = %v, want ErrNotFound", err)
	}

	// Statistics survive a restart, and a later update replaces them.
	c = reopen(t, c, dir)
	if info, _ := c.Lookup("users"); info.Pages != 3 || info.Tuples != 250 {
		t.Errorf("after reopen: %+v", info)
	}
	if err := c.UpdateStats(oid, 4, 300, 21); err != nil {
		t.Fatal(err)
	}
	if info, _ := c.Lookup("users"); info.Pages != 4 || info.Tuples != 300 {
		t.Errorf("after second update: %+v", info)
	}
	infos, _ := c.Tables()
	if got := names(infos); !reflect.DeepEqual(got, []string{"orders", "pg_attribute", "pg_class", "pg_index", "users", "users_pkey"}) {
		t.Errorf("Tables = %v", got)
	}
}

// Chapter 16: the control file has two writers, the catalog for OIDs and
// the transaction manager for XIDs; neither may lose the other's update.
func TestUpdateControl(t *testing.T) {
	dir := t.TempDir()
	if err := UpdateControl(dir, func(c *Control) {}); !errors.Is(err, ErrNotBootstrapped) {
		t.Fatalf("UpdateControl on empty dir: %v, want ErrNotBootstrapped", err)
	}
	start := Control{NextOID: FirstUserOID, NextXID: tuple.FirstNormalXID}
	if err := WriteControl(dir, start); err != nil {
		t.Fatal(err)
	}
	const workers, each = 8, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				err := UpdateControl(dir, func(c *Control) {
					if w%2 == 0 {
						c.NextOID++
					} else {
						c.NextXID++
					}
				})
				if err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()
	got, err := ReadControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Control{NextOID: start.NextOID + workers/2*each, NextXID: start.NextXID + workers/2*each}
	if got != want {
		t.Errorf("control after concurrent updates = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "global", "control.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("control.tmp left behind: %v", err)
	}
}

// Chapter 17: catalog snapshots.

// fakeSnap is a heap.Snapshot that hides the rows of the transactions in
// aborted, and of the transactions in running unless they are its own.
type fakeSnap struct {
	own              tuple.XID
	aborted, running map[tuple.XID]bool
}

func (f *fakeSnap) committed(xid tuple.XID) bool {
	return xid == f.own || !f.aborted[xid] && !f.running[xid]
}

func (f *fakeSnap) Visible(t tuple.Tuple) (bool, error) {
	if !f.committed(t.Xmin()) {
		return false, nil
	}
	return t.Xmax() == tuple.InvalidXID || !f.committed(t.Xmax()), nil
}

func (f *fakeSnap) Dirty(t tuple.Tuple) (bool, tuple.XID, error) {
	v, err := f.Visible(t)
	return v, 0, err
}

func (f *fakeSnap) Modify(t tuple.Tuple, tid tuple.TID) (heap.TM, error) {
	switch {
	case !f.committed(t.Xmin()):
		return heap.TMInvisible, nil
	case t.Xmax() == tuple.InvalidXID || f.aborted[t.Xmax()]:
		return heap.TMOk, nil
	case t.Xmax() == f.own:
		return heap.TMSelfModified, nil
	case f.running[t.Xmax()]:
		return heap.TMBeingModified, nil
	}
	return heap.TMDeleted, nil
}

func (f *fakeSnap) Wait(xid tuple.XID) {}

func TestCatalogSnapshot(t *testing.T) {
	c, _ := bootstrap(t)
	snap := &fakeSnap{own: 30, aborted: map[tuple.XID]bool{}, running: map[tuple.XID]bool{}}
	c.SetSnapshot(func() heap.Snapshot { return snap })
	if _, err := c.CreateTable("users", usersDesc, 30); err != nil {
		t.Fatal(err)
	}
	// Own uncommitted DDL is visible to the session itself.
	if _, err := c.Lookup("users"); err != nil {
		t.Fatalf("own table: %v", err)
	}
	// To another transaction it is not, running or aborted.
	other := &fakeSnap{own: 31, aborted: map[tuple.XID]bool{}, running: map[tuple.XID]bool{30: true}}
	c.SetSnapshot(func() heap.Snapshot { return other })
	if _, err := c.Lookup("users"); !errors.Is(err, ErrNotFound) {
		t.Errorf("table of a running transaction: %v, want ErrNotFound", err)
	}
	other.running[30], other.aborted[30] = false, true
	if err := c.EndTransaction(false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lookup("users"); !errors.Is(err, ErrNotFound) {
		t.Errorf("table of an aborted transaction: %v, want ErrNotFound", err)
	}
	if infos, err := c.Tables(); err != nil || len(infos) != 3 {
		t.Errorf("Tables() = %d relations, %v; want the 3 catalogs", len(infos), err)
	}
	// The name is free: the aborted row does not count as a duplicate.
	if _, err := c.CreateTable("users", usersDesc, 31); err != nil {
		t.Errorf("CreateTable after an aborted one: %v", err)
	}
	if err := c.EndTransaction(true); err != nil {
		t.Fatal(err)
	}
	// A committed drop hides the table; an aborted drop does not, and
	// keeps the file.
	other.aborted[32] = true
	if err := c.DropTable("users", 32); err != nil {
		t.Fatal(err)
	}
	if err := c.EndTransaction(false); err != nil {
		t.Fatal(err)
	}
	info, err := c.Lookup("users")
	if err != nil {
		t.Fatalf("table after an aborted drop: %v", err)
	}
	if exists, err := c.Pool().Store().Exists(info.OID); err != nil || !exists {
		t.Errorf("file after an aborted drop: exists=%v err=%v", exists, err)
	}
	if err := c.DropTable("users", 33); err != nil {
		t.Fatal(err)
	}
	if err := c.EndTransaction(true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lookup("users"); !errors.Is(err, ErrNotFound) {
		t.Errorf("table after a committed drop: %v", err)
	}
	if exists, err := c.Pool().Store().Exists(info.OID); err != nil || exists {
		t.Errorf("file after a committed drop: exists=%v err=%v", exists, err)
	}
}

func TestEndTransactionAbort(t *testing.T) {
	c, _ := bootstrap(t)
	oid, err := c.CreateTable("users", usersDesc, 30)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := c.CreateIndex("users_name", oid, 1, false, false, 30)
	if err != nil {
		t.Fatal(err)
	}
	// Abort removes the files the transaction created; the catalog rows
	// stay, stamped with the aborted ID, for the snapshot to hide.
	if err := c.EndTransaction(false); err != nil {
		t.Fatal(err)
	}
	for _, o := range []tuple.OID{oid, idx.OID} {
		if exists, err := c.Pool().Store().Exists(o); err != nil || exists {
			t.Errorf("file %d after abort: exists=%v err=%v", o, exists, err)
		}
	}
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); len(got) != 5 {
		t.Errorf("pg_class has %d rows after abort, want 5", len(got))
	}
	// Created and dropped in one transaction: the file goes either way,
	// and only once.
	oid, err = c.CreateTable("orders", ordersDesc, 31)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DropTable("orders", 31); err != nil {
		t.Fatal(err)
	}
	if err := c.EndTransaction(true); err != nil {
		t.Fatal(err)
	}
	if exists, err := c.Pool().Store().Exists(oid); err != nil || exists {
		t.Errorf("file created and dropped in one transaction: exists=%v err=%v", exists, err)
	}
	// Nothing pending: EndTransaction is a no-op.
	if err := c.EndTransaction(false); err != nil {
		t.Errorf("EndTransaction with nothing pending: %v", err)
	}
}

func TestInvalidate(t *testing.T) {
	c, _ := bootstrap(t)
	if _, err := c.CreateTable("users", usersDesc, 30); err != nil {
		t.Fatal(err)
	}
	first, err := c.Lookup("users")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := c.Lookup("users"); again != first {
		t.Error("Lookup did not cache")
	}
	c.Invalidate()
	if again, _ := c.Lookup("users"); again == first {
		t.Error("Invalidate kept the cached entry")
	}
	// SetSnapshot invalidates too.
	first, _ = c.Lookup("users")
	c.SetSnapshot(nil)
	if again, _ := c.Lookup("users"); again == first {
		t.Error("SetSnapshot kept the cached entry")
	}
}
