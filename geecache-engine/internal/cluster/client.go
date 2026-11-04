package cluster

import (
	"net/http"
	"time"
)

type Client struct {
	address string
	http    *http.Client
	timeout time.Duration
	token   string
}
