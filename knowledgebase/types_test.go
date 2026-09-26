// SPDX-License-Identifier: Apache-2.0

package knowledgebase

import (
	"strings"
	"testing"
)

func TestCatalogJSONRendersSortedRecords(t *testing.T) {
	catalog := &Catalog{}
	catalog.Add(Record{ID: "zeta", Paths: []string{"z.md"}})
	catalog.Add(Record{ID: "alpha", Paths: []string{"a.md"}})
	catalog.Sort()
	body, err := catalog.JSON()
	if err != nil || !strings.Contains(string(body), `"id": "alpha"`) || strings.Index(string(body), `"id": "alpha"`) > strings.Index(string(body), `"id": "zeta"`) {
		t.Fatalf("catalog JSON = %q, err=%v", body, err)
	}
}
