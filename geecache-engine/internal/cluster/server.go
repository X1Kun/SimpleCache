package cluster

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/X1Kun/simpleCache/geecache-engine/internal/cache"
)

type Handler struct {
	groups map[string]*cache.Group
	token  string
}

func NewHandler(groups ...*cache.Group) *Handler {
	return NewHandlerWithToken("", groups...)
}

func NewHandlerWithToken(token string, groups ...*cache.Group) *Handler {
	h := &Handler{groups: make(map[string]*cache.Group), token: token}
	for _, g := range groups {
		if _, exists := h.groups[g.Name()]; exists {
			panic("duplicate group")
		}
		h.groups[g.Name()] = g
	}
	return h
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete || h.token == "" {
		w.Header().Set("Allow", "DELETE")
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.Method == http.MethodDelete && !Authorized(r, h.token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	path := r.URL.EscapedPath()
	if !strings.HasPrefix(path, BasePath) {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(path, BasePath), "/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad member path", 400)
		return
	}
	group, e1 := url.PathUnescape(parts[0])
	key, e2 := url.PathUnescape(parts[1])
	if e1 != nil || e2 != nil {
		http.Error(w, "bad encoding", 400)
		return
	}
	g, ok := h.groups[group]
	if !ok {

		http.Error(w, "group not found", 404)
		return
	}
	if r.Method == http.MethodDelete {
		version, err := ParseInvalidationVersion(r.Header)
		if err == nil {
			err = g.InvalidateVersion(key, version)
		}
		if err != nil {
			http.Error(w, err.Error(), Status(err))
			return
		}
		if version > 0 {
			w.Header().Set(InvalidationVersionHeader, strconv.FormatInt(version, 10))
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
}
