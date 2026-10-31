package resources

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func unixBackend(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "clustat-api-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "client.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		_ = os.RemoveAll(directory)
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Add(1)
			go func() { defer wg.Done(); defer func() { _ = conn.Close() }(); handle(conn) }()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for _, conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
		_ = os.RemoveAll(directory)
	})
	return path
}

func TestBackendV110UUIDJSONAndCollectorIntegration(t *testing.T) {
	requests := make(chan string, 2)
	path := unixBackend(t, func(conn net.Conn) {
		line, _ := bufio.NewReader(conn).ReadString('\n')
		requests <- line
		_, _ = io.WriteString(conn, `{"meta":{"status":"ok","timestamp_ms":42,"node_count":1},"nodes":[{"hostname":"node-a","timestamp_ms":42,"stale":false,"gres":[{"index":5,"uuid":"GPU-A","util":80,"mem_used_mb":2048,"mem_total_mb":8192},{"index":1,"uuid":null,"util":50,"mem_used_mb":0,"mem_total_mb":8192}]}]}`+"\n")
	})
	b := Backend{Socket: path, Timeout: time.Second}
	nodes, err := b.Read(context.Background(), []string{"node-a", "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || len(nodes[0].Devices) != 2 || nodes[0].Devices[0].UUID == nil || *nodes[0].Devices[0].UUID != "GPU-A" || nodes[0].Devices[1].UUID != nil {
		t.Fatalf("v1.1.0 UUID parsing failed %+v", nodes)
	}
	line := <-requests
	if !strings.HasPrefix(line, "QUERY ") || !strings.HasSuffix(line, "\n") {
		t.Fatalf("wrong local protocol %q", line)
	}
	var query struct {
		Filter string `json:"filter"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(line, "QUERY ")), &query) != nil || query.Filter != "node-a" {
		t.Fatalf("query not bounded to unique node %q", line)
	}
	c := startCollector(t, []Device{{"node-a", "GPU-A"}}, 1)
	c.Observe(epoch().Add(time.Second), nodes)
	usage := c.Finish("job", epoch().Add(time.Second), 1)
	near(t, *usage.MeanUtilizationPercent, 80)
	near(t, *usage.MeanMemoryUtilizationPercent, 25)
}

func TestBackendTimeoutAndCancellationInterruptBlockedRead(t *testing.T) {
	path := unixBackend(t, func(conn net.Conn) { _, _ = bufio.NewReader(conn).ReadString('\n'); _, _ = io.Copy(io.Discard, conn) })
	started := time.Now()
	if _, err := (Backend{Socket: path, Timeout: 10 * time.Millisecond}).Read(context.Background(), []string{"node-a"}); err == nil {
		t.Fatal("blocked backend did not time out")
	}
	if time.Since(started) > time.Second {
		t.Fatal("backend deadline ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (Backend{Socket: path, Timeout: time.Minute}).Read(ctx, []string{"node-a"})
		done <- err
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not interrupt backend read")
	}
}

func TestBackendRejectsIncompleteOversizedAndInvalidResponses(t *testing.T) {
	for _, body := range []string{`{}`, `{"nodes":null}` + "\n", `{"nodes":[]}`, `{"nodes":[]}garbage` + "\n", strings.Repeat("x", (4<<20)+1) + "\n"} {
		t.Run("response", func(t *testing.T) {
			path := unixBackend(t, func(conn net.Conn) { _, _ = bufio.NewReader(conn).ReadString('\n'); _, _ = io.WriteString(conn, body) })
			if _, err := (Backend{Socket: path, Timeout: time.Second}).Read(context.Background(), []string{"node-a"}); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
}

func TestBackendNoGPUAndInvalidFiltersDoNotQuery(t *testing.T) {
	b := Backend{}
	if nodes, err := b.Read(context.Background(), nil); err != nil || len(nodes) != 0 {
		t.Fatalf("CPU-only job attempted backend read %v", err)
	}
	for _, filter := range []string{"", "node*", "node[1-4]", "node-a,node-b", "node-a\nQUERY {}", "node a"} {
		if _, err := b.Read(context.Background(), []string{filter}); err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("filter %q was not rejected before connecting: %v", filter, err)
		}
	}
}
