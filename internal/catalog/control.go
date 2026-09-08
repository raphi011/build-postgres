package catalog

import "github.com/raphi011/build-postgres/internal/tuple"

// Control is the content of <datadir>/global/control.
type Control struct {
	NextOID tuple.OID
	NextXID tuple.XID
}

// ReadControl reads the control file of the data directory at dir.
// Returns ErrNotBootstrapped if the file does not exist; ErrControlCorrupt
// if it is not exactly 20 bytes or its magic, version, or CRC is wrong.
// PostgreSQL: ReadControlFile in xlog.c.
func ReadControl(dir string) (Control, error) {
	panic("not implemented")
}

// WriteControl atomically replaces the control file of the data directory
// at dir. Creates global/ if missing; writes control.tmp, syncs it, and
// renames it over control, so a crash leaves the old or the new file.
// PostgreSQL: update_controlfile in controldata_utils.c.
func WriteControl(dir string, c Control) error {
	panic("not implemented")
}
