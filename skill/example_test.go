// SPDX-License-Identifier: Apache-2.0

package skill_test

import (
	"fmt"

	"github.com/threadgrid/threadpoint/skill"
)

func ExampleParseFrontmatter() {
	metadata, err := skill.ParseFrontmatter([]byte(`---
name: release-check
description: Verify a release candidate.
---
`))
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s: %s\n", metadata.Name, metadata.Description)

	// Output:
	// release-check: Verify a release candidate.
}
