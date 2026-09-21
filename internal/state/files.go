package state

import (
	"errors"

	"os"

	"regexp"

	"syscall"
)

var (
	SystemIDPattern   = regexp.MustCompile(`^sys_[a-f0-9]{32}$`)
	CommandKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
)

// openOwnedFile refuses symlinks, foreign ownership, and writable-by-others
// files. State files must additionally be private. Checking the opened inode
// avoids trusting a separate stat of a potentially replaced final path.
func OpenOwnedFile(filename string, flags int, private bool) (*os.File, error) {
	// Nonblocking open lets us reject FIFOs instead of hanging before fstat.
	// It does not change regular-file reads or writes.
	file, err := os.OpenFile(filename, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Geteuid()) ||
		info.Mode().Perm()&0022 != 0 || (private && info.Mode().Perm()&0077 != 0) {
		return nil, errors.Join(errors.New("file must be regular, owned by this user, and have safe permissions"), file.Close())
	}
	return file, nil
}
