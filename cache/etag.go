package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// etagFor returns an RFC 9110 entity-tag for body. Entity-tags are quoted;
// when weak is true the validator is emitted as a weak ETag (W/"...").
func etagFor(body []byte, weak bool) string {
	sum := sha256.Sum256(body)
	tag := `"` + hex.EncodeToString(sum[:]) + `"`
	if weak {
		return "W/" + tag
	}
	return tag
}

// etagMatches reports whether an If-None-Match value matches etag, in which
// case the response should be 304 Not Modified.
//
// If-None-Match uses the weak comparison function (RFC 9110 §13.1.2), so
// W/"x" matches "x"; "*" matches any current representation.
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" || etag == "" {
		return false
	}

	for _, candidate := range splitETags(ifNoneMatch) {
		if candidate == "*" {
			return true
		}
		if weakETagEqual(candidate, etag) {
			return true
		}
	}
	return false
}

// splitETags splits a comma-separated entity-tag list. Commas inside a quoted
// opaque-tag are legal, so a plain strings.Split is not sufficient.
func splitETags(value string) []string {
	var (
		tags    []string
		inQuote bool
		start   int
	)

	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			inQuote = !inQuote
		case ',':
			if inQuote {
				continue
			}
			if tag := strings.TrimSpace(value[start:i]); tag != "" {
				tags = append(tags, tag)
			}
			start = i + 1
		}
	}

	if tag := strings.TrimSpace(value[start:]); tag != "" {
		tags = append(tags, tag)
	}
	return tags
}

// weakETagEqual compares two entity-tags with the weak comparison function:
// the opaque-tags must match, but the W/ prefix is ignored.
func weakETagEqual(a, b string) bool {
	return strings.TrimPrefix(strings.TrimSpace(a), "W/") ==
		strings.TrimPrefix(strings.TrimSpace(b), "W/")
}
