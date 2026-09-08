package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/raphi011/build-postgres/internal/page"
)

// sampleDump is what dump prints for the relation sample creates.
const sampleDump = `16384: 1 block

block 0
  lsn 0  lower 36  upper 8072  special 8192  free 8032  items 3
    n   offset    len flags      xmin   xmax     ctid  natts  hoff  infomask
    1     8152     37 normal        1      0    (0,1)      2    24    0x0000
    2     8112     36 normal        1      0    (0,2)      2    24    0x0000
    3     8072     37 normal        1      0    (0,3)      2    24    0x0000
`

func TestSampleThenDump(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := sample(dir, &out); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "base", strconv.Itoa(sampleOID))
	if got := out.String(); !strings.HasSuffix(got, file+"\n") {
		t.Errorf("sample printed %q, want the path %q", got, file)
	}
	out.Reset()
	if err := dump(file, -1, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != sampleDump {
		t.Errorf("dump printed\n%s\nwant\n%s", got, sampleDump)
	}
}

func TestDumpNotAPageFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "short")
	if err := os.WriteFile(file, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	err := dump(file, -1, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "not a multiple") {
		t.Errorf("dump of a 100-byte file: %v, want a size error", err)
	}
}

func TestDumpNewPage(t *testing.T) {
	file := filepath.Join(t.TempDir(), "16384")
	if err := os.WriteFile(file, make([]byte, page.PageSize), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := dump(file, -1, &out); err != nil {
		t.Fatal(err)
	}
	want := "16384: 1 block\n\nblock 0\n  new: all zero bytes, never initialised\n"
	if got := out.String(); got != want {
		t.Errorf("dump of a zero-filled page printed\n%s\nwant\n%s", got, want)
	}
}

func TestDumpCorruptHeader(t *testing.T) {
	buf := make([]byte, page.PageSize)
	for i := range buf {
		buf[i] = 0xFF
	}
	file := filepath.Join(t.TempDir(), "16384")
	if err := os.WriteFile(file, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := dump(file, -1, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "corrupt") {
		t.Errorf("dump of a 0xFF page printed\n%s\nwant a corrupt header line", got)
	}
}

func TestDumpItemPointerOutOfRange(t *testing.T) {
	p := page.New(0)
	// One normal line pointer whose item runs past the end of the page:
	// offset 8100, length 200. The header stays valid.
	binary.LittleEndian.PutUint16(p[12:], page.HeaderSize+page.LinePointerSize)
	binary.LittleEndian.PutUint32(p[page.HeaderSize:], 8100|1<<15|200<<17)
	file := filepath.Join(t.TempDir(), "16384")
	if err := os.WriteFile(file, p, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := dump(file, -1, &out); err != nil {
		t.Fatal(err)
	}
	want := "    1     8100    200 normal        -      -        -      -     -         -\n"
	if got := out.String(); !strings.Contains(got, want) {
		t.Errorf("dump printed\n%s\nwant a line %q", got, want)
	}
}

func TestDumpBlockOutOfRange(t *testing.T) {
	dir := t.TempDir()
	if err := sample(dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "base", strconv.Itoa(sampleOID))
	var out bytes.Buffer
	err := dump(file, 7, &out)
	if err == nil || !strings.Contains(err.Error(), "no block 7") {
		t.Errorf("dump of block 7 of a one-block file: %v, want a range error", err)
	}
	if out.Len() != 0 {
		t.Errorf("dump printed %q, want nothing", out.String())
	}
}

func TestSampleNeedsAFreshDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("18\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := sample(dir, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("sample into a non-empty directory: %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "base")); !os.IsNotExist(err) {
		t.Errorf("sample wrote base/ into a non-empty directory")
	}
}
