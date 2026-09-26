// SPDX-License-Identifier: Apache-2.0

// Package safepath holds threadpoint's shared filename and path sanitization
// helpers, kept in one place so safety rules cannot drift between packages and
// embedding applications.
package safepath

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrUnsafe marks a relative path that would escape its destination root.
var ErrUnsafe = errors.New("unsafe relative path")

// SanitizeName reduces a single path component to lowercase [a-z0-9._-], mapping
// any other rune to '-' and trimming surrounding dots and dashes. It may return
// the empty string; callers that need a guaranteed non-empty component should
// use SanitizeSegment or provide their own default.
func SanitizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, ".")
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	clean := strings.Trim(b.String(), "-")
	// Reject components that reduce to only dots (".", "..", "..."): joined to a
	// destination they would be interpreted as the current/parent directory and
	// allow traversal. e.g. "!..!" would otherwise sanitize to "..".
	if strings.Trim(clean, ".") == "" {
		return ""
	}
	return clean
}

// SanitizeSegment is SanitizeName with a non-empty guarantee: it returns
// "imported" when sanitization would otherwise yield an empty component.
func SanitizeSegment(name string) string {
	if clean := SanitizeName(name); clean != "" {
		return clean
	}
	return "imported"
}

// SanitizeRelative sanitizes each component of a relative path, dropping leading
// parent or root markers, so the result always stays within a destination
// directory. It returns "artifact" for an empty or "." path.
func SanitizeRelative(rel string) string {
	rel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
	rel = strings.TrimPrefix(rel, "../")
	rel = strings.TrimPrefix(rel, "/")
	if rel == "." || rel == "" {
		return "artifact"
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		parts[i] = SanitizeSegment(part)
	}
	return filepath.ToSlash(filepath.Join(parts...))
}

// Join joins rel onto root, rejecting absolute paths and parent-directory
// traversal so the result cannot escape root.
func Join(root string, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("unsafe relative path %q: %w", rel, ErrUnsafe)
	}
	return filepath.Join(root, clean), nil
}

// IsWithin reports whether path is equal to root or contained below root after
// filepath.Clean normalization.
func IsWithin(path string, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
