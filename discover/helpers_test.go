// SPDX-License-Identifier: Apache-2.0

package discover

import (
	"io/fs"
	"time"
)

type boundaryFileInfo struct {
	mode fs.FileMode
}

func (i boundaryFileInfo) Name() string { return "fixture" }

func (i boundaryFileInfo) Size() int64 { return 0 }

func (i boundaryFileInfo) Mode() fs.FileMode { return i.mode }

func (i boundaryFileInfo) ModTime() time.Time { return time.Time{} }

func (i boundaryFileInfo) IsDir() bool { return i.mode.IsDir() }

func (i boundaryFileInfo) Sys() any { return nil }
