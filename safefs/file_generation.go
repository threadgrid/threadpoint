// SPDX-License-Identifier: Apache-2.0

package safefs

import (
	"io/fs"
	"os"
)

// SameFileGeneration supplements os.SameFile with platform metadata to catch
// inode-reuse ABA changes. Use it only where the object should remain unchanged;
// Linux change time rejects in-place metadata or content mutations too.
func SameFileGeneration(expected, current fs.FileInfo) bool {
	return expected != nil && current != nil && os.SameFile(expected, current) && sameFileGenerationMetadata(expected, current)
}
