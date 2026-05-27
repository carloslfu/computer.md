// SPDX-License-Identifier: Apache-2.0

package vault

import (
	"regexp"
	"strings"
)

// ReferencePattern matches $SECRET_NAME and ${SECRET_NAME} patterns.
var ReferencePattern = regexp.MustCompile(`\$\{?([A-Z][A-Z0-9_]*)\}?`)

// Resolver handles $SECRET_NAME -> real value resolution in text.
type Resolver struct {
	store *Store
}

// NewResolver creates a resolver backed by the given vault store.
func NewResolver(store *Store) *Resolver {
	return &Resolver{store: store}
}

// Resolve replaces all $SECRET_NAME references in text with their vault values.
// References that don't match any secret are left unchanged.
func (r *Resolver) Resolve(text string) string {
	return ReferencePattern.ReplaceAllStringFunc(text, func(match string) string {
		// Extract the name from $NAME or ${NAME}.
		name := match
		name = strings.TrimPrefix(name, "${")
		name = strings.TrimSuffix(name, "}")
		name = strings.TrimPrefix(name, "$")

		value, ok := r.store.Get(name)
		if !ok {
			// Not a known secret, leave the reference as-is.
			return match
		}
		return value
	})
}

// ResolveMap takes a map of strings and resolves references in all values.
func (r *Resolver) ResolveMap(m map[string]string) map[string]string {
	resolved := make(map[string]string, len(m))
	for k, v := range m {
		resolved[k] = r.Resolve(v)
	}
	return resolved
}

// HasReferences checks if text contains any $SECRET_NAME references.
func HasReferences(text string) bool {
	return ReferencePattern.MatchString(text)
}

// ExtractReferences returns all secret names referenced in the text.
func ExtractReferences(text string) []string {
	matches := ReferencePattern.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool)
	var names []string
	for _, match := range matches {
		if len(match) > 1 && !seen[match[1]] {
			names = append(names, match[1])
			seen[match[1]] = true
		}
	}
	return names
}

// ResolveEnvLine resolves references in a KEY=VALUE environment line.
// Only the value part is resolved.
func (r *Resolver) ResolveEnvLine(line string) string {
	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 {
		return line
	}
	return parts[0] + "=" + r.Resolve(parts[1])
}
