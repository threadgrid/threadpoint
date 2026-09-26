// SPDX-License-Identifier: Apache-2.0

package diagnostic

import (
	"testing"
)

func TestValidCode(t *testing.T) {
	for _, code := range []string{"license_state_missing", "discovery_environment_file", "update_signature_invalid"} {
		if !ValidCode(code) {
			t.Fatalf("ValidCode(%q) = false", code)
		}
	}
	for _, code := range []string{"", "threadpoint_error", "diagnostic.code", "layout-invalid", "UPPER_CASE"} {
		if ValidCode(code) {
			t.Fatalf("ValidCode(%q) = true", code)
		}
	}
}
