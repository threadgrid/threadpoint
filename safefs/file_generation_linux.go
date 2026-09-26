// SPDX-License-Identifier: Apache-2.0

//go:build linux

package safefs

import (
	"io/fs"
	"syscall"
)

func sameFileGenerationMetadata(expected, current fs.FileInfo) bool {
	expectedStat, expectedOK := expected.Sys().(*syscall.Stat_t)
	currentStat, currentOK := current.Sys().(*syscall.Stat_t)
	return expectedOK && currentOK &&
		expectedStat.Ctim.Sec == currentStat.Ctim.Sec &&
		expectedStat.Ctim.Nsec == currentStat.Ctim.Nsec
}
