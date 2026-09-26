// SPDX-License-Identifier: Apache-2.0

// Package editor opens the user's configured terminal editor on a temporary
// copy of some content and returns the edited result.
package editor

import (
	"context"

	"github.com/threadgrid/threadpoint/review"
)

// Open writes initial to a temporary file, opens it in $VISUAL then $EDITOR,
// and returns the edited contents. It deliberately does not select an editor
// when neither variable is configured, so a staged private artifact is never
// opened by an unexpected program.
func Open(ctx context.Context, streams review.Streams, initial string) (string, error) {
	body, err := review.Edit(ctx, streams, "threadpoint-edit.md", []byte(initial))
	if err != nil {
		return "", err
	}
	return string(body), nil
}
