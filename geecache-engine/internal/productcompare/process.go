package productcompare

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type node struct {
	api, peer, name string
	pid             int
	capacity        int64
	command         *exec.Cmd
	done            chan error
	running         bool
}

type cluster struct {
	nodes                                    []*node
	options                                  Options
	mode, dir, dsn, token, applicationPrefix string
}

func startCluster(ctx context.Context, o Options, mode, dir, dsn, token, applicationPrefix string) (*cluster, error) {
	c := &cluster{options: o, mode: mode, dir: dir, dsn: dsn, token: token, applicationPrefix: applicationPrefix}
	var reserved []net.Listener
	for i := 0; i < 6; i++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			for _, r := range reserved {
				_ = r.Close()
			}
			return nil, err
		}
		reserved = append(reserved, listener)
	}
	for i := 0; i < 3; i++ {
		c.nodes = append(c.nodes, &node{name: nodeIDs[i], peer: reserved[i*2].Addr().String(), api: "http://" + reserved[i*2+1].Addr().String(), capacity: o.CacheBytes})
	}
	for _, listener := range reserved {
		_ = listener.Close()
	}
	// Ports are released immediately before the children bind. If another process
	// wins a bind, child startup fails before any API traffic is sent.
	for i := range c.nodes {
		if err := c.startNode(ctx, i, "initial"); err != nil {
			return nil, errors.Join(err, c.stop())
		}
	}
	return c, nil
}

func (c *cluster) startNode(ctx context.Context, i int, suffix string) error {
	n := c.nodes[i]
	var addresses []string
	for _, peer := range c.nodes {
		addresses = append(addresses, peer.peer)
	}
	log, err := os.Create(filepath.Join(c.dir, fmt.Sprintf("%s-%s.log", n.name, suffix)))
	if err != nil {
		return err
	}
	cmd := exec.Command(c.options.Server, "-mode", c.mode, "-node", strconv.Itoa(i), "-peers", strings.Join(addresses, ","), "-api", strings.TrimPrefix(n.api, "http://"), "-capacity", strconv.FormatInt(c.options.CacheBytes, 10))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key != "GOMAXPROCS" && key != "DATABASE_URL" && key != "CACHE_ADMIN_TOKEN" && key != "SIMPLECACHE_COMPARE_DATABASE_URL" {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	dsn, err := databaseURL(c.dsn, "", c.applicationPrefix+"-"+n.name)
	if err != nil {
		_ = log.Close()
		return err
	}
	cmd.Env = append(cmd.Env, "GOMAXPROCS=2", "DATABASE_URL="+dsn, "CACHE_ADMIN_TOKEN="+c.token)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = log.Close()
		return err
	}
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return err
	}
	n.command, n.pid, n.running = cmd, cmd.Process.Pid, true
	n.done = make(chan error, 1)
	ready := make(chan map[string]string, 1)
	go func() {
		defer log.Close()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			_, _ = fmt.Fprintln(log, scanner.Text())
			var event struct {
				Message   string            `json:"msg"`
				Listeners map[string]string `json:"listeners"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Message == "Fixture ready" {
				select {
				case ready <- event.Listeners:
				default:
				}
			}
		}
		n.done <- errors.Join(cmd.Wait(), scanner.Err())
	}()
	select {
	case addresses := <-ready:
		if addresses["api"] != strings.TrimPrefix(n.api, "http://") || addresses["peer"] != n.peer {
			return errors.New("fixture reported unexpected bound addresses")
		}
		return nil
	case err := <-n.done:
		n.running = false
		return fmt.Errorf("%s exited before startup: %v", n.name, err)
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
		return fmt.Errorf("%s startup timed out", n.name)
	}
}

func (n *node) stop() error {
	if !n.running {
		return nil
	}
	n.running = false
	_ = n.command.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-n.done:
		return err
	case <-time.After(8 * time.Second):
		_ = n.command.Process.Kill()
		<-n.done
		return errors.New("fixture exceeded graceful shutdown budget")
	}
}

func (c *cluster) stop() error {
	var result error
	for _, n := range c.nodes {
		result = errors.Join(result, n.stop())
	}
	return result
}
