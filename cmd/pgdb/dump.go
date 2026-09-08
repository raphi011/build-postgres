// The sample and dump subcommands. sample writes a small heap relation to
// a fresh data directory; dump prints the page header, the line pointers,
// and, for a heap page, the tuple headers of a relation file. Both are
// provided complete: they are a debugging tool, not an exercise. See
// chapters/02-tuples.

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/raphi011/build-postgres/internal/page"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// sampleOID is the relation sample creates.
const sampleOID = 16384

// sample writes a data directory holding one relation of three rows and
// prints the path of its file. It writes the page itself rather than going
// through the storage manager, so that it works from chapter 02 on; the
// layout is the one chapter 03 will produce.
//
// dir must not exist or be empty: writing base/16384 into a real data
// directory would replace a table the catalog still describes.
func sample(dir string, out io.Writer) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s: not empty, sample needs a fresh directory", dir)
	}
	desc := tuple.NewDesc(
		tuple.Attr{Name: "a", Type: tuple.Int4},
		tuple.Attr{Name: "b", Type: tuple.Text},
	)
	rows := []tuple.Datum{int32(1), "alpha", int32(2), "beta", int32(3), "gamma"}
	p := page.New(0)
	for i := 0; i < len(rows); i += 2 {
		t, err := tuple.Form(desc, rows[i:i+2], nil)
		if err != nil {
			return err
		}
		t.SetXmin(1)
		t.SetXmax(tuple.InvalidXID)
		off, err := p.AddItem(t)
		if err != nil {
			return err
		}
		item, err := p.GetItem(off)
		if err != nil {
			return err
		}
		tuple.Tuple(item).SetCtid(tuple.TID{Block: 0, Off: off})
	}
	file := filepath.Join(dir, "base", strconv.Itoa(sampleOID))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(file, p, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "%d rows of (a int4, b text) in %s\n", len(rows)/2, file)
	return nil
}

// dump prints every block of the relation file at path, or only blk when
// it is not negative.
func dump(path string, blk int, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size()%page.PageSize != 0 {
		return fmt.Errorf("%s: %d bytes is not a multiple of %d",
			path, info.Size(), page.PageSize)
	}
	nblocks := int(info.Size() / page.PageSize)
	if blk >= nblocks {
		return fmt.Errorf("%s: no block %d, the file has %d block%s",
			path, blk, nblocks, map[bool]string{false: "s", true: ""}[nblocks == 1])
	}
	fmt.Fprintf(out, "%s: %d block%s\n", filepath.Base(path), nblocks,
		map[bool]string{false: "s", true: ""}[nblocks == 1])

	buf := make([]byte, page.PageSize)
	for b := 0; b < nblocks; b++ {
		if blk >= 0 && b != blk {
			continue
		}
		if _, err := f.ReadAt(buf, int64(b)*page.PageSize); err != nil {
			return err
		}
		dumpPage(out, b, page.Page(buf))
	}
	return nil
}

// dumpPage prints one page: the header, then a line per line pointer with
// the tuple header behind it. A page that was extended but never
// initialised, and one whose header offsets are out of range, are reported
// rather than walked: their item count is meaningless.
func dumpPage(out io.Writer, blk int, p page.Page) {
	fmt.Fprintf(out, "\nblock %d\n", blk)
	if p.IsNew() {
		fmt.Fprintf(out, "  new: all zero bytes, never initialised\n")
		return
	}
	fmt.Fprintf(out, "  lsn %d  lower %d  upper %d  special %d  free %d  items %d\n",
		p.LSN(), p.Lower(), p.Upper(), p.Special(), p.FreeSpace(), p.NumItems())
	if !validHeader(p) {
		fmt.Fprintf(out, "  corrupt: the header offsets are out of range\n")
		return
	}
	if p.NumItems() == 0 {
		return
	}
	heapPage := p.Special() == page.PageSize
	if !heapPage {
		fmt.Fprintf(out, "  %3s %8s %6s %s\n", "n", "offset", "len", "flags")
		for n := page.OffsetNumber(1); n <= p.NumItems(); n++ {
			id := p.ItemID(n)
			fmt.Fprintf(out, "  %3d %8d %6d %s\n", n, id.Off, id.Len, flagName(id.Flags))
		}
		return
	}
	fmt.Fprintf(out, "  %3s %8s %6s %-8s %6s %6s %8s %6s %5s %9s\n",
		"n", "offset", "len", "flags", "xmin", "xmax", "ctid", "natts", "hoff", "infomask")
	for n := page.OffsetNumber(1); n <= p.NumItems(); n++ {
		id := p.ItemID(n)
		fmt.Fprintf(out, "  %3d %8d %6d %-8s", n, id.Off, id.Len, flagName(id.Flags))
		item, ok := heapItem(p, n, id)
		if !ok {
			fmt.Fprintf(out, " %6s %6s %8s %6s %5s %9s\n", "-", "-", "-", "-", "-", "-")
			continue
		}
		t := tuple.Tuple(item)
		ctid := t.Ctid()
		fmt.Fprintf(out, " %6d %6d %8s %6d %5d %9s\n",
			t.Xmin(), t.Xmax(), fmt.Sprintf("(%d,%d)", ctid.Block, ctid.Off),
			t.Natts(), t.Hoff(), fmt.Sprintf("0x%04x", t.Infomask()))
	}
}

// validHeader reports whether the header offsets are in range, so that the
// line pointer array and the items behind it lie inside the page.
func validHeader(p page.Page) bool {
	lower, upper, special := int(p.Lower()), int(p.Upper()), int(p.Special())
	return lower >= page.HeaderSize && lower <= upper && upper <= special &&
		special <= page.PageSize && (lower-page.HeaderSize)%page.LinePointerSize == 0
}

// heapItem returns the item behind line pointer id, or false when it does
// not point at a whole tuple header inside the page.
func heapItem(p page.Page, n page.OffsetNumber, id page.ItemID) ([]byte, bool) {
	if int(id.Off) < page.HeaderSize || int(id.Off)+int(id.Len) > page.PageSize {
		return nil, false
	}
	item, err := p.GetItem(n)
	if err != nil || len(item) < tuple.FixedHeaderSize {
		return nil, false
	}
	return item, true
}

// flagName is the line pointer state as pageinspect prints it.
func flagName(f page.ItemFlags) string {
	switch f {
	case page.ItemUnused:
		return "unused"
	case page.ItemNormal:
		return "normal"
	case page.ItemRedirect:
		return "redirect"
	default:
		return "dead"
	}
}
