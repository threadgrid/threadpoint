// SPDX-License-Identifier: Apache-2.0

package clierror

import (
	"errors"
	"fmt"
	"testing"
)

func TestTaggedErrorsPreserveCauseAndKind(t *testing.T) {
	cause := errors.New("confirmation declined")
	for _, test := range []struct {
		err  error
		kind Kind
	}{
		{err: Refused(cause), kind: KindRefused},
		{err: Usage(cause), kind: KindUsage},
	} {
		var classified *Error
		if !errors.As(test.err, &classified) || classified.Kind != test.kind || !errors.Is(test.err, cause) {
			t.Fatalf("classified error = %#v", test.err)
		}
	}
	if Refused(nil) != nil || Usage(nil) != nil {
		t.Fatal("nil errors should remain nil")
	}
}

func TestClassificationSurvivesWrapping(t *testing.T) {
	refused := Refused(errors.New("nope"))
	var ce *Error
	if !errors.As(refused, &ce) || ce.Kind != KindRefused {
		t.Fatalf("expected KindRefused, got %#v", ce)
	}

	wrapped := fmt.Errorf("context: %w", Usage(errors.New("bad flag")))
	ce = nil
	if !errors.As(wrapped, &ce) || ce.Kind != KindUsage {
		t.Fatalf("expected KindUsage through %%w wrap, got %#v", ce)
	}

	if Refused(nil) != nil || Usage(nil) != nil {
		t.Fatal("nil error must stay nil")
	}
	if got := refused.Error(); got != "nope" {
		t.Fatalf("unexpected message: %q", got)
	}
}
