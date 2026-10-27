package app

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

func versionETag(version int64) string { return `"v` + strconv.FormatInt(version, 10) + `"` }

// This API deliberately accepts one exact strong source-version ETag for
// replacement/deletion. Wildcard If-Match would permit stale overwrites.
// Zero is reserved for PUT with If-None-Match: * (create-if-absent).
func mutationPrecondition(r *http.Request) (int64, int, error) {
	matches, absent := r.Header.Values("If-Match"), r.Header.Values("If-None-Match")
	if len(matches) == 0 && len(absent) == 0 {
		return 0, http.StatusPreconditionRequired, errors.New("use If-Match with the GET ETag, or If-None-Match: * to create")
	}
	bad := errors.New("require one exact If-Match ETag, or PUT with If-None-Match: *; do not combine them")
	if len(matches) == 0 && len(absent) == 1 && r.Method == http.MethodPut && strings.TrimSpace(absent[0]) == "*" {
		return 0, 0, nil
	}
	if len(matches) != 1 || len(absent) != 0 {
		return 0, http.StatusBadRequest, bad
	}
	raw := strings.TrimSpace(matches[0])
	if len(raw) < 4 || !strings.HasPrefix(raw, `"v`) || !strings.HasSuffix(raw, `"`) {
		return 0, http.StatusBadRequest, bad
	}
	version, err := strconv.ParseInt(raw[2:len(raw)-1], 10, 64)
	if err != nil || version <= 0 || raw != versionETag(version) {
		return 0, http.StatusBadRequest, bad
	}
	return version, 0, nil
}
