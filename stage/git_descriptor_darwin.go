// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package stage

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// descriptorLocation reports where the directory held by file is reachable now,
// following any rename since it was opened, and its device:inode identity in
// the format `stat -f '%d:%i'` prints. Darwin's /dev/fd entries cannot be
// entered and report the descriptor filesystem's device rather than the
// directory's, so the Git child enters this path and compares its working
// directory against the identity read here from the retained descriptor.
func descriptorLocation(file *os.File) (path string, identity string, err error) {
	var buf [unix.PathMax]byte
	// #nosec G103 -- F_GETPATH writes at most PathMax bytes into buf. The pointer
	// is converted in the argument list of an assembly-implemented syscall, which
	// keeps buf alive and unmoved until the call returns.
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, file.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return "", "", errno
	}
	current, _, _ := bytes.Cut(buf[:], []byte{0})
	if len(current) == 0 {
		return "", "", errors.New("retained directory path is unavailable")
	}
	info, err := file.Stat()
	if err != nil {
		return "", "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", "", errors.New("retained directory identity is unavailable")
	}
	// BSD stat prints the 32-bit st_dev unsigned.
	return string(current), fmt.Sprintf("%d:%d", uint32(stat.Dev), stat.Ino), nil
}
