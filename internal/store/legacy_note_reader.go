package store

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// SQL substr() can materialise an entire old TEXT value inside SQLite. Migration
// uses incremental BLOB reads instead, on a stable source: either the database
// whose writer transaction we hold, or a privately staged restore snapshot.
type legacyBodySource struct {
	q      Queryer
	tls    *libc.TLS
	db     uintptr
	buffer uintptr
	slot   uintptr
}

func (s *legacyBodySource) open() error {
	if s.tls != nil {
		return nil
	}
	var sequence int
	var name, path string
	if err := s.q.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		return err
	}
	if path == "" {
		return failure("storage_invalid", "legacy notes require a file-backed source")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	s.tls = libc.NewTLS()
	s.slot = sqlite3.Xsqlite3_malloc64(s.tls, strconv.IntSize/8)
	s.buffer = sqlite3.Xsqlite3_malloc64(s.tls, noteChunkBytes)
	if s.slot == 0 || s.buffer == 0 {
		s.Close()
		return failure("storage_io", "cannot allocate legacy note read buffer")
	}
	filename, err := libc.CString(path)
	if err != nil {
		s.Close()
		return err
	}
	libc.AtomicStoreNUintptr(s.slot, 0, 0)
	rc := sqlite3.Xsqlite3_open_v2(s.tls, filename, s.slot, sqlite3.SQLITE_OPEN_READONLY, 0)
	libc.Xfree(s.tls, filename)
	s.db = libc.AtomicLoadNUintptr(s.slot, 0)
	if rc != sqlite3.SQLITE_OK {
		err = s.error(rc)
		s.Close()
		return err
	}
	query, err := libc.CString("PRAGMA trusted_schema=OFF; PRAGMA busy_timeout=5000; BEGIN")
	if err != nil {
		s.Close()
		return err
	}
	rc = sqlite3.Xsqlite3_exec(s.tls, s.db, query, 0, 0, 0)
	libc.Xfree(s.tls, query)
	if rc != sqlite3.SQLITE_OK {
		err = s.error(rc)
		s.Close()
		return err
	}
	return nil
}

func (s *legacyBodySource) error(code int32) error {
	message := fmt.Sprintf("legacy SQLite read failed (%d)", code)
	if s.db != 0 {
		message += ": " + libc.GoString(sqlite3.Xsqlite3_errmsg(s.tls, s.db))
	}
	if code&0xff == sqlite3.SQLITE_BUSY || code&0xff == sqlite3.SQLITE_LOCKED {
		return failure("busy", message)
	}
	return failure("storage_invalid", message)
}

func (s *legacyBodySource) Close() {
	if s.tls == nil {
		return
	}
	if s.db != 0 {
		sqlite3.Xsqlite3_close_v2(s.tls, s.db)
	}
	sqlite3.Xsqlite3_free(s.tls, s.buffer)
	sqlite3.Xsqlite3_free(s.tls, s.slot)
	s.tls.Close()
	s.tls, s.db, s.buffer, s.slot = nil, 0, 0, 0
}

func (s *legacyBodySource) reader(table, column string, id, length int64) (*legacyBlobReader, error) {
	if err := s.open(); err != nil {
		return nil, err
	}
	var names [3]uintptr
	defer func() {
		for _, name := range names {
			libc.Xfree(s.tls, name)
		}
	}()
	for i, name := range []string{"main", table, column} {
		p, err := libc.CString(name)
		if err != nil {
			return nil, err
		}
		names[i] = p
	}
	libc.AtomicStoreNUintptr(s.slot, 0, 0)
	rc := sqlite3.Xsqlite3_blob_open(s.tls, s.db, names[0], names[1], names[2], id, 0, s.slot)
	if rc != sqlite3.SQLITE_OK {
		return nil, s.error(rc)
	}
	r := &legacyBlobReader{source: s, blob: libc.AtomicLoadNUintptr(s.slot, 0), length: length}
	if int64(sqlite3.Xsqlite3_blob_bytes(s.tls, r.blob)) != length {
		r.Close()
		return nil, failure("storage_invalid", "legacy note changed while migrating")
	}
	return r, nil
}

type legacyBlobReader struct {
	source   *legacyBodySource
	blob     uintptr
	position int64
	length   int64
}

func (r *legacyBlobReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.position == r.length {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), noteChunkBytes, r.length-r.position))
	s := r.source
	if rc := sqlite3.Xsqlite3_blob_read(s.tls, r.blob, s.buffer, int32(n), int32(r.position)); rc != sqlite3.SQLITE_OK {
		return 0, s.error(rc)
	}
	copy(p, libc.GoBytes(s.buffer, n))
	r.position += int64(n)
	return n, nil
}

func (r *legacyBlobReader) Close() {
	if r.blob != 0 {
		sqlite3.Xsqlite3_blob_close(r.source.tls, r.blob)
		r.blob = 0
	}
}

func (r *legacyBlobReader) prefix() (string, error) {
	var prefix [512]byte
	n, err := io.ReadFull(r, prefix[:])
	r.position = 0
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		err = nil
	}
	return string(prefix[:n]), err
}
