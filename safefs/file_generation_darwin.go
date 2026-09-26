// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package safefs

import (
	"io/fs"
	"syscall"
)

func sameFileGenerationMetadata(expected, current fs.FileInfo) bool {
	expectedStat, expectedOK := expected.Sys().(*syscall.Stat_t)
	currentStat, currentOK := current.Sys().(*syscall.Stat_t)
	if !expectedOK || !currentOK || expectedStat.Gen != currentStat.Gen {
		return false
	}
	if expectedStat.Birthtimespec.Sec != 0 || expectedStat.Birthtimespec.Nsec != 0 ||
		currentStat.Birthtimespec.Sec != 0 || currentStat.Birthtimespec.Nsec != 0 {
		return expectedStat.Birthtimespec.Sec == currentStat.Birthtimespec.Sec &&
			expectedStat.Birthtimespec.Nsec == currentStat.Birthtimespec.Nsec
	}
	return expectedStat.Ctimespec.Sec == currentStat.Ctimespec.Sec &&
		expectedStat.Ctimespec.Nsec == currentStat.Ctimespec.Nsec
}
