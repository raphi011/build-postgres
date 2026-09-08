package catalog

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"

	"github.com/raphi011/build-postgres/internal/smgr"
	"github.com/raphi011/build-postgres/internal/tuple"
)

// Control file layout: magic, version, next OID, next XID, CRC-32.
const (
	controlMagic   = "PGDB"
	controlVersion = 1
	controlSize    = 20
)

func controlPath(dir string) string {
	return filepath.Join(dir, "global", "control")
}

// ReadControl reads the control file of the data directory at dir.
// Returns ErrNotBootstrapped if the file does not exist; ErrControlCorrupt
// if it is not exactly 20 bytes or its magic, version, or CRC is wrong.
// PostgreSQL: ReadControlFile in xlog.c.
func ReadControl(dir string) (Control, error) {
	b, err := os.ReadFile(controlPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return Control{}, ErrNotBootstrapped
	}
	if err != nil {
		return Control{}, err
	}
	if len(b) != controlSize ||
		string(b[0:4]) != controlMagic ||
		binary.LittleEndian.Uint32(b[4:8]) != controlVersion ||
		binary.LittleEndian.Uint32(b[16:20]) != crc32.ChecksumIEEE(b[:16]) {
		return Control{}, ErrControlCorrupt
	}
	return Control{
		NextOID: tuple.OID(binary.LittleEndian.Uint32(b[8:12])),
		NextXID: tuple.XID(binary.LittleEndian.Uint32(b[12:16])),
	}, nil
}

// WriteControl atomically replaces the control file of the data directory
// at dir. Creates global/ if missing; writes control.tmp, syncs it,
// renames it over control, and syncs global/ so the rename itself is
// durable, so a crash leaves the old or the new file.
// PostgreSQL: update_controlfile in controldata_utils.c.
func WriteControl(dir string, c Control) error {
	b := make([]byte, controlSize)
	copy(b[0:4], controlMagic)
	binary.LittleEndian.PutUint32(b[4:8], controlVersion)
	binary.LittleEndian.PutUint32(b[8:12], uint32(c.NextOID))
	binary.LittleEndian.PutUint32(b[12:16], uint32(c.NextXID))
	binary.LittleEndian.PutUint32(b[16:20], crc32.ChecksumIEEE(b[:16]))

	path := controlPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// The rename is only durable once the directory entry is.
	return smgr.SyncDir(filepath.Dir(path))
}

// controlMu serialises read-modify-write cycles of the control file: the
// catalog updates NextOID and the transaction manager NextXID, and each
// must see the other's last write.
// PostgreSQL: ControlFileLock.
var controlMu sync.Mutex

// UpdateControl reads the control file, lets fn change it, and writes it
// back, all under a process-wide lock. Returns ReadControl's and
// WriteControl's errors.
// PostgreSQL: UpdateControlFile under ControlFileLock in xlog.c.
func UpdateControl(dir string, fn func(*Control)) error {
	controlMu.Lock()
	defer controlMu.Unlock()
	c, err := ReadControl(dir)
	if err != nil {
		return err
	}
	fn(&c)
	return WriteControl(dir, c)
}
