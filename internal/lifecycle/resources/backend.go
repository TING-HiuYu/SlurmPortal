package resources

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"
)

// Backend reads clustat v1.1.0's existing local API. It never invokes a shell or
// nvidia-smi and never opens a second connection to a compute-node collector.
type Backend struct {
	Socket  string
	Timeout time.Duration
}

func (b Backend) Read(ctx context.Context, nodes []string) ([]NodeSample, error) {
	if len(nodes) == 0 {
		return []NodeSample{}, nil
	}
	if len(nodes) > 4096 {
		return nil, errors.New("clustat node query exceeds capacity")
	}
	unique := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if node == "" || len(node) > 255 || strings.ContainsAny(node, ",[]*?\r\n \t") {
			return nil, errors.New("invalid clustat node identity")
		}
		unique[node] = true
	}
	filtered := make([]string, 0, len(unique))
	for node := range unique {
		filtered = append(filtered, node)
	}
	sort.Strings(filtered)
	if b.Socket == "" {
		return nil, errors.New("clustat backend socket is not configured")
	}
	timeout := b.Timeout
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", b.Socket)
	if err != nil {
		return nil, errors.New("clustat backend unavailable")
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	payload, _ := json.Marshal(struct {
		Filter string `json:"filter"`
	}{strings.Join(filtered, ",")})
	if _, err = fmt.Fprintf(conn, "QUERY %s\n", payload); err != nil {
		return nil, errors.New("clustat query failed")
	}
	const limit = 4 << 20
	line, err := bufio.NewReader(io.LimitReader(conn, limit+1)).ReadBytes('\n')
	if err != nil || len(line) > limit {
		return nil, errors.New("clustat response incomplete or oversized")
	}
	var response struct {
		Nodes []NodeSample `json:"nodes"`
	}
	if json.Unmarshal(line, &response) != nil || response.Nodes == nil || len(response.Nodes) > 4096 {
		return nil, errors.New("invalid clustat response")
	}
	return response.Nodes, nil
}
