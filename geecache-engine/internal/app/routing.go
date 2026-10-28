package app

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

// Envoy hashes the wire query value. Accept exactly one canonical representation
// so percent-encoding aliases, duplicate parameters and '+' cannot split one
// decoded source key across cache shards. Legacy Peer mode keeps its old API.
func requestKey(r *http.Request, canonical bool) (string, error) {
	if !canonical {
		return r.URL.Query().Get("key"), nil
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	bad := errors.New("gateway mode requires one canonical key query: percent-encoded UTF-8, uppercase escapes, %20 for spaces")
	if err != nil || len(values) != 1 || len(values["key"]) != 1 {
		return "", bad
	}
	key := values["key"][0]
	if !utf8.ValidString(key) {
		return "", bad
	}
	if err := cache.ValidateKey(key); err != nil {
		return "", err
	}
	wire := strings.ReplaceAll(url.QueryEscape(key), "+", "%20")
	if r.URL.RawQuery != "key="+wire {
		return "", bad
	}
	return key, nil
}
