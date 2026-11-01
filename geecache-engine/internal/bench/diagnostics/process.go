package diagnostics

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type service struct {
	api, debug string
	pid        int
	stop       func() error
}

func startServer(ctx context.Context, binary, dir string, capacity int64, valueBytes int, contention bool) (service, error) {
	log, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		return service{}, err
	}
	cmd := exec.Command(binary, "-capacity", strconv.FormatInt(capacity, 10), "-value-bytes", strconv.Itoa(valueBytes), "-contention="+strconv.FormatBool(contention))
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GOMAXPROCS=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GOMAXPROCS=2")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Close()
		return service{}, err
	}
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		log.Close()
		return service{}, err
	}
	addresses := make(chan map[string]string, 1)
	done := make(chan error, 1)
	go func() {
		defer log.Close()
		scan := bufio.NewScanner(stdout)
		scan.Buffer(make([]byte, 4096), 1<<20)
		for scan.Scan() {
			fmt.Fprintln(log, scan.Text())
			var event struct {
				Message   string            `json:"msg"`
				Listeners map[string]string `json:"listeners"`
			}
			if json.Unmarshal(scan.Bytes(), &event) == nil && event.Message == "Cache server started" {
				select {
				case addresses <- event.Listeners:
				default:
				}
			}
		}
		done <- cmd.Wait()
	}()
	stop := func() error {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			return err
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			return fmt.Errorf("server exceeded graceful shutdown budget")
		}
	}
	select {
	case a := <-addresses:
		if a["api"] == "" || a["debug"] == "" {
			_ = stop()
			return service{}, fmt.Errorf("missing bound server addresses")
		}
		return service{api: "http://" + a["api"], debug: "http://" + a["debug"], pid: cmd.Process.Pid, stop: stop}, nil
	case err := <-done:
		return service{}, fmt.Errorf("server exited before startup: %v", err)
	case <-time.After(15 * time.Second):
		_ = stop()
		return service{}, fmt.Errorf("server startup timed out")
	case <-ctx.Done():
		_ = stop()
		return service{}, ctx.Err()
	}
}
