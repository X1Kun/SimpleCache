package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type runner struct {
	c        Config
	client   *http.Client
	token    string
	expected [][]byte
}

func (r *runner) request(ctx context.Context, index, keyIndex int, method string, expected, min, max int64) Record {
	out := Record{Index: index, KeyIndex: keyIndex, Method: method, Attempted: true, CommittedAtStart: min}
	key := Key(r.c.Prefix, keyIndex)
	var body io.Reader
	if method == "PUT" {
		body = bytes.NewReader(Value(key, expected+1, r.c.ValueBytes))
	}
	started := time.Now()
	req, e := http.NewRequestWithContext(ctx, method, r.c.Endpoint+"/api?key="+strings.ReplaceAll(url.QueryEscape(key), "+", "%20"), body)
	if e != nil {
		out.Error = "request construction failed"
		return out
	}
	req.Host = r.c.Host
	if method == "PUT" {
		req.Header.Set("Authorization", "Bearer "+r.token)
		req.Header.Set("If-Match", `"v`+strconv.FormatInt(expected, 10)+`"`)
		req.Header.Set("Content-Type", "application/json")
	}
	response, e := r.client.Do(req)
	if e != nil {
		out.ElapsedMS = float64(time.Since(started)) / float64(time.Millisecond)
		out.Error = "transport or deadline failure"
		return out
	}
	data, e := io.ReadAll(io.LimitReader(response.Body, int64(r.c.ValueBytes+4097)))
	_ = response.Body.Close()
	out.ElapsedMS = float64(time.Since(started)) / float64(time.Millisecond)
	out.Status = response.StatusCode
	out.Node = response.Header.Get("X-SimpleCache-Node")
	if e != nil {
		out.Error = "response read failed"
		return out
	}
	if response.StatusCode != 200 {
		out.Error = "non-200 response"
		return out
	}
	if method == "PUT" {
		var result struct {
			Committed    *bool `json:"source_committed"`
			Version      int64 `json:"source_version"`
			Invalidation struct {
				Complete bool                            `json:"complete"`
				Version  int64                           `json:"version"`
				Results  []struct{ Node, Status string } `json:"results"`
			} `json:"invalidation"`
		}
		if json.Unmarshal(data, &result) != nil || result.Committed == nil || !*result.Committed || result.Version != expected+1 || result.Invalidation.Version != result.Version || !result.Invalidation.Complete || len(result.Invalidation.Results) != 3 {
			out.Incorrect = true
		} else {
			seen := map[string]bool{}
			for _, ack := range result.Invalidation.Results {
				if ack.Status != "ok" || seen[ack.Node] {
					out.Incorrect = true
				}
				seen[ack.Node] = true
			}
		}
		out.Version = result.Version
		return out
	}
	var value Payload
	if json.Unmarshal(data, &value) != nil || value.Key != key || value.Version < 1 {
		out.Incorrect = true
		return out
	}
	out.Version = value.Version
	if out.Node == "" {
		out.Incorrect = true
	}
	if response.Header.Get("ETag") != `"v`+strconv.FormatInt(value.Version, 10)+`"` || value.Version > max {
		out.Incorrect = true
	}
	if value.Version < min {
		out.StaleAtStart = true
		out.Incorrect = true
	}
	var wanted []byte
	if value.Version == 1 {
		wanted = r.expected[keyIndex]
	} else {
		wanted = Value(key, value.Version, r.c.ValueBytes)
	}
	if !bytes.Equal(data, wanted) {
		out.Incorrect = true
	}
	return out
}
