// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package safefs

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const (
	renameRootNoReplaceSupported = true
	renameRootExchangeSupported  = true
)

func renameRootNoReplaceAt(oldParent *os.File, oldBase string, newParent *os.File, newBase string) error {
	err := unix.RenameatxNp(int(oldParent.Fd()), oldBase, int(newParent.Fd()), newBase, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("%w: %w", ErrRenameNoReplaceUnsupported, err)
	}
	return err
}

func renameRootExchangeAt(firstParent *os.File, firstBase string, secondParent *os.File, secondBase string) error {
	err := unix.RenameatxNp(int(firstParent.Fd()), firstBase, int(secondParent.Fd()), secondBase, unix.RENAME_SWAP)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("%w: %w", ErrRenameExchangeUnsupported, err)
	}
	return err
}
