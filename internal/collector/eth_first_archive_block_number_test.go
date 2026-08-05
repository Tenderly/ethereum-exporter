package collector

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"
)

// fakeNode serves the subset of JSON-RPC the archive search uses, retaining
// state only from firstArchive onwards. It counts eth_getBalance calls so tests
// can assert on probe cost.
type fakeNode struct {
	head         uint64
	firstArchive uint64 // 0 means the node serves no state at all
	probes       int
}

func (n *fakeNode) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []string        `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("could not decode request: %#v", err)
			return
		}

		reply := func(format string, args ...interface{}) {
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,`+format+`}`, append([]interface{}{req.ID}, args...)...)
			if _, err := w.Write([]byte(body)); err != nil {
				t.Errorf("could not write a response: %#v", err)
			}
		}

		switch req.Method {
		case "web3_clientVersion":
			reply(`"result":"fake/v1.0.0"`)

		case "eth_blockNumber":
			reply(`"result":%q`, hexutil.EncodeUint64(n.head))

		case "eth_getBalance":
			n.probes++

			block, err := hexutil.DecodeUint64(req.Params[1])
			if err != nil {
				t.Errorf("could not decode block %q: %#v", req.Params[1], err)
				return
			}
			// Geth answers a pruned height with an error, not a null result.
			if n.firstArchive == 0 || block < n.firstArchive || block > n.head {
				reply(`"error":{"code":-32000,"message":"missing trie node"}`)
				return
			}
			reply(`"result":"0x0"`)

		default:
			reply(`"error":{"code":-32601,"message":"method not found"}`)
		}
	}))
}

func TestEthFirstArchiveBlockNumberFindFirstArchiveBlock(t *testing.T) {
	tests := []struct {
		name         string
		head         uint64
		firstArchive uint64
		wantProbes   int
	}{
		{name: "full archive from genesis", head: 16_000_000, firstArchive: 1, wantProbes: 1},
		{name: "pruned node", head: 16_000_000, firstArchive: 15_500_000},
		{name: "state only at head", head: 16_000_000, firstArchive: 16_000_000},
		{name: "small chain", head: 4_200, firstArchive: 3_000},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			node := &fakeNode{head: test.head, firstArchive: test.firstArchive}
			server := node.server(t)
			defer server.Close()

			client, err := rpc.DialHTTP(server.URL)
			if err != nil {
				t.Fatalf("rpc connection error: %#v", err)
			}

			collector := newEthFirstArchiveBlockNumber(client)
			got, probes, err := collector.findFirstArchiveBlock(test.head)
			if err != nil {
				t.Fatalf("unexpected error: %#v", err)
			}
			if got != test.firstArchive {
				t.Fatalf("got %d, want %d", got, test.firstArchive)
			}
			if test.wantProbes > 0 && probes != test.wantProbes {
				t.Fatalf("got %d probes, want %d", probes, test.wantProbes)
			}
		})
	}
}

// When the head block is unknown the search falls back to the hard-coded
// candidates, which bound it only if one of them happens to be retained. A node
// whose retained range sits entirely between two candidates cannot be bounded,
// so the search reports no state rather than guessing. This matches proverator.
func TestEthFirstArchiveBlockNumberWithoutHead(t *testing.T) {
	tests := []struct {
		name         string
		head         uint64
		firstArchive uint64
		want         uint64
		wantErr      bool
	}{
		{name: "candidate is retained", head: 16_000_000, firstArchive: 9_000_000, want: 9_000_000},
		{name: "no candidate is retained", head: 16_000_000, firstArchive: 15_500_000, wantErr: true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			node := &fakeNode{head: test.head, firstArchive: test.firstArchive}
			server := node.server(t)
			defer server.Close()

			client, err := rpc.DialHTTP(server.URL)
			if err != nil {
				t.Fatalf("rpc connection error: %#v", err)
			}

			collector := newEthFirstArchiveBlockNumber(client)
			got, _, err := collector.findFirstArchiveBlock(0)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got block %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %#v", err)
			}
			if got != test.want {
				t.Fatalf("got %d, want %d", got, test.want)
			}
		})
	}
}

func TestEthFirstArchiveBlockNumberNoState(t *testing.T) {
	node := &fakeNode{head: 16_000_000}
	server := node.server(t)
	defer server.Close()

	client, err := rpc.DialHTTP(server.URL)
	if err != nil {
		t.Fatalf("rpc connection error: %#v", err)
	}

	collector := newEthFirstArchiveBlockNumber(client)
	if _, _, err := collector.findFirstArchiveBlock(16_000_000); err == nil {
		t.Fatal("expected an error, got none")
	}
}

func TestEthFirstArchiveBlockNumberCollect(t *testing.T) {
	node := &fakeNode{head: 16_000_000, firstArchive: 15_500_000}
	server := node.server(t)
	defer server.Close()

	client, err := rpc.DialHTTP(server.URL)
	if err != nil {
		t.Fatalf("rpc connection error: %#v", err)
	}

	collector := newEthFirstArchiveBlockNumber(client)
	collector.refresh()

	ch := make(chan prometheus.Metric, 1)
	collector.Collect(ch)
	close(ch)

	if got := len(ch); got != 1 {
		t.Fatalf("got %v, want 1", got)
	}

	var metric dto.Metric
	for result := range ch {
		if err := result.Write(&metric); err != nil {
			t.Fatalf("expected metric, got %#v", err)
		}
		if got := len(metric.Label); got > 0 {
			t.Fatalf("expected 0 labels, got %d", got)
		}
		if got := *metric.Gauge.Value; got != 15_500_000 {
			t.Fatalf("got %v, want 15500000", got)
		}
	}
}

// Until the first search completes, Collect must report an error rather than a
// zero block number.
func TestEthFirstArchiveBlockNumberCollectPending(t *testing.T) {
	client, err := rpc.DialHTTP("http://localhost")
	if err != nil {
		t.Fatalf("rpc connection error: %#v", err)
	}

	collector := newEthFirstArchiveBlockNumber(client)
	ch := make(chan prometheus.Metric, 1)

	collector.Collect(ch)
	close(ch)

	if got := len(ch); got != 1 {
		t.Fatalf("got %v, want 1", got)
	}

	for result := range ch {
		var metric dto.Metric
		if err := result.Write(&metric); err != errSearchPending {
			t.Fatalf("got %#v, want errSearchPending", err)
		}
	}
}

func TestEthFirstArchiveBlockNumberCollectError(t *testing.T) {
	client, err := rpc.DialHTTP("http://localhost")
	if err != nil {
		t.Fatalf("rpc connection error: %#v", err)
	}

	collector := newEthFirstArchiveBlockNumber(client)
	collector.refresh()

	ch := make(chan prometheus.Metric, 1)
	collector.Collect(ch)
	close(ch)

	if got := len(ch); got != 1 {
		t.Fatalf("got %v, want 1", got)
	}

	for result := range ch {
		var metric dto.Metric
		err := result.Write(&metric)
		if err == nil {
			t.Fatalf("expected invalid metric, got %#v", metric)
		}
		if _, ok := err.(*url.Error); ok {
			t.Fatalf("unexpected error %#v", err)
		}
	}
}
