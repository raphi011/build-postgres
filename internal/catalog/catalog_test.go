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
	s := rel.Scan()
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

	wantClass := [][]tuple.Datum{
		classRow(ClassOID, "pg_class"),
		classRow(AttributeOID, "pg_attribute"),
	}
	if got := rows(t, pool, ClassOID, ClassDesc); !reflect.DeepEqual(got, wantClass) {
		t.Fatalf("pg_class rows\n got %v\nwant %v", got, wantClass)
	}
	wantAttr := append(attrRows(ClassOID, ClassDesc), attrRows(AttributeOID, AttributeDesc)...)
	if got := rows(t, pool, AttributeOID, AttributeDesc); !reflect.DeepEqual(got, wantAttr) {
		t.Fatalf("pg_attribute rows\n got %v\nwant %v", got, wantAttr)
	}

	// Rows are stamped with the bootstrap XID.
	s := heap.Open(pool, ClassOID, ClassDesc).Scan()
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
	}{{"pg_class", ClassOID, ClassDesc}, {"pg_attribute", AttributeOID, AttributeDesc}} {
		info, err := c.Lookup(tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if info.OID != tc.oid || info.Name != tc.name || info.Kind != RelKindTable {
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
	if got := rows(t, c2.Pool(), ClassOID, ClassDesc); len(got) != 2 {
		t.Fatalf("pg_class has %d rows after reopen, want 2", len(got))
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
	wantClass := [][]tuple.Datum{
		classRow(ClassOID, "pg_class"),
		classRow(AttributeOID, "pg_attribute"),
		classRow(oid, "users"),
		classRow(oid2, "orders"),
	}
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); !reflect.DeepEqual(got, wantClass) {
		t.Fatalf("pg_class rows\n got %v\nwant %v", got, wantClass)
	}
	wantAttr := attrRows(ClassOID, ClassDesc)
	wantAttr = append(wantAttr, attrRows(AttributeOID, AttributeDesc)...)
	wantAttr = append(wantAttr, attrRows(oid, usersDesc)...)
	wantAttr = append(wantAttr, attrRows(oid2, ordersDesc)...)
	if got := rows(t, c.Pool(), AttributeOID, AttributeDesc); !reflect.DeepEqual(got, wantAttr) {
		t.Fatalf("pg_attribute rows\n got %v\nwant %v", got, wantAttr)
	}
	s := heap.Open(c.Pool(), ClassOID, ClassDesc).Scan()
	defer s.Close()
	var xmins []tuple.XID
	for s.Next() {
		xmins = append(xmins, s.Tuple().Xmin())
	}
	if want := []tuple.XID{1, 1, 10, 11}; !reflect.DeepEqual(xmins, want) {
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
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); len(got) != 3 {
		t.Fatalf("pg_class has %d rows, want 3", len(got))
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
	want := []string{"apple", "mango", "pg_attribute", "pg_class", "zebra"}
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
	if got := names(infos); !reflect.DeepEqual(got, []string{"pg_attribute", "pg_class", "users"}) {
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
	if exists, err := c.Pool().Store().Exists(oid); err != nil || exists {
		t.Fatalf("relation file after drop: exists=%v err=%v", exists, err)
	}
	wantClass := [][]tuple.Datum{
		classRow(ClassOID, "pg_class"),
		classRow(AttributeOID, "pg_attribute"),
		classRow(oid+1, "orders"),
	}
	if got := rows(t, c.Pool(), ClassOID, ClassDesc); !reflect.DeepEqual(got, wantClass) {
		t.Fatalf("pg_class rows after drop\n got %v\nwant %v", got, wantClass)
	}
	wantAttr := attrRows(ClassOID, ClassDesc)
	wantAttr = append(wantAttr, attrRows(AttributeOID, AttributeDesc)...)
	wantAttr = append(wantAttr, attrRows(oid+1, ordersDesc)...)
	if got := rows(t, c.Pool(), AttributeOID, AttributeDesc); !reflect.DeepEqual(got, wantAttr) {
		t.Fatalf("pg_attribute rows after drop\n got %v\nwant %v", got, wantAttr)
	}

	// The deleted rows are still there, stamped with the dropping XID.
	rel := heap.Open(c.Pool(), ClassOID, ClassDesc)
	old, err := rel.Fetch(tuple.TID{Block: 0, Off: 3})
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
	if got := names(infos); !reflect.DeepEqual(got, []string{"orders", "pg_attribute", "pg_class", "users"}) {
		t.Fatalf("Tables after reopen = %v", got)
	}
}

func TestDropTableErrors(t *testing.T) {
	c, _ := bootstrap(t)
	if err := c.DropTable("nope", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DropTable(nope) = %v, want ErrNotFound", err)
	}
	for _, name := range []string{"pg_class", "pg_attribute"} {
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
