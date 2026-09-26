// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package stage

import (
	"errors"
	"os"
)

// descriptorLocation is only needed where descriptor paths cannot be entered;
// other systems enter the descriptor path directly.
func descriptorLocation(*os.File) (string, string, error) {
	return "", "", errors.New("descriptor location lookup is only used on darwin")
}
