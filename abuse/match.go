package abuse

import "strings"

// matchPath reports whether a request path matches a rule pattern.
// The grammar is:
//   - exact match: the pattern equals the path (e.g., "/.env");
//   - trailing "*" wildcard: the "*" matches any remainder of the path, but
//     only on a path-segment boundary. "/admin*" matches "/admin", "/admin/"
//     and "/admin/config" but not "/administrator"; "/api/v1/*" matches
//     "/api/v1/users/1" but not "/api/v1evil". Mid-pattern wildcards such as
//     "/api/*/x" are not supported and are matched literally.
//
// Matching is case-sensitive and operates on URL.Path only, so query strings
// and fragments never take part.
func matchPath(pattern, path string) bool {
	// 1. Exact match
	if pattern == path {
		return true
	}

	// 2. Trailing "*" wildcard, matched on a path-segment boundary
	if strings.HasSuffix(pattern, "*") {
		prefix := strings.TrimSuffix(pattern, "*")
		if !strings.HasPrefix(path, prefix) {
			return false
		}
		// The "*" may only continue at a segment boundary: either the pattern
		// already ends at a "/" ("/api/v1/*"), or the next character of the
		// path must start a new segment ("/admin", "/admin/").
		if strings.HasSuffix(prefix, "/") {
			return true
		}
		rest := path[len(prefix):]
		return rest == "" || strings.HasPrefix(rest, "/")
	}

	return false
}
