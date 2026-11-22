//go:build !unix

package store

func (s *Store) noteLock(shared bool) (func(), error) {
	return nil, failure("unsupported_platform", "note storage requires Unix file locks")
}
