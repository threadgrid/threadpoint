// SPDX-License-Identifier: Apache-2.0

package layout_test

import (
	"fmt"
	"os"

	"github.com/threadgrid/threadpoint/layout"
)

func ExampleValidate() {
	root, err := os.MkdirTemp("", "threadpoint-layout-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)

	if err := layout.EnsureShared(root); err != nil {
		panic(err)
	}
	report := layout.Validate(root)
	fmt.Println(report.OK)

	// Output:
	// true
}
