// SPDX-License-Identifier: Apache-2.0

package knowledgebase_test

import (
	"fmt"

	"github.com/threadgrid/threadpoint/knowledgebase"
)

func ExampleCatalog() {
	catalog := &knowledgebase.Catalog{}
	catalog.Add(knowledgebase.Record{ID: "rules/security", Kind: knowledgebase.RecordRule})
	catalog.Add(knowledgebase.Record{ID: "knowledge/project", Kind: knowledgebase.RecordKnowledge})
	catalog.Sort()

	for _, record := range catalog.Records {
		fmt.Println(record.ID)
	}

	// Output:
	// knowledge/project
	// rules/security
}
