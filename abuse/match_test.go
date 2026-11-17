package abuse

import "testing"

// TestMatchPath is a table-driven test for the path matching grammar used by
// abuse rules: exact matches, and a trailing "*" wildcard that only matches on
// a path-segment boundary.
func TestMatchPath(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		// Exact matches.
		{"exact hit", "/.env", "/.env", true},
		{"exact miss", "/.env", "/.env.bak", false},
		{"exact miss different path", "/.env", "/other", false},
		{"empty pattern empty path", "", "", true},

		// Trailing "*" on a segment boundary: subtree semantics.
		{"subtree prefix", "/api/v1/*", "/api/v1/users/1", true},
		{"subtree root itself", "/admin*", "/admin", true},
		{"subtree trailing slash", "/admin*", "/admin/", true},
		{"subtree child", "/admin*", "/admin/config", true},
		{"no segment bleed", "/admin*", "/administrator", false},
		{"no segment bleed v1", "/api/v1*", "/api/v1evil", false},
		{"subtree slash-star no bleed", "/api/v1/*", "/api/v1evil", false},
		{"root wildcard matches all absolute paths", "/*", "/", true},
		{"root wildcard child", "/*", "/anything/else", true},
		{"root wildcard not relative", "/*", "relative", false},

		// Matched paths come from URL.Path, so query strings and fragments
		// must never influence the outcome.
		{"query string not matched", "/admin*", "/admin?x=1", false},

		// Only trailing "*" is supported; mid-pattern wildcards are literal.
		{"mid wildcard unsupported", "/api/*/x", "/api/v1/x", false},
		{"star in middle literal", "/api/*/x", "/api/*/x", true},

		// Matching is case-sensitive (paths are matched verbatim).
		{"case sensitive", "/ADMIN*", "/admin", false},

		// No pattern at all.
		{"non wildcard no match", "/exact", "/exact/sub", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchPath(tt.pattern, tt.path); got != tt.want {
				t.Errorf("matchPath(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}
