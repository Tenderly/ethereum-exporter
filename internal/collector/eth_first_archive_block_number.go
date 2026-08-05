package collector

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/prometheus/client_golang/prometheus"
)

// zeroAddress is the account probed with eth_getBalance to decide whether the
// node still serves state at a given height. Any address works, the zero
// address is just guaranteed to be resolvable in every state trie.
const zeroAddress = "0x0000000000000000000000000000000000000000"

// archiveProbeCandidates seed the upper bound of the binary search when the head
// block is unknown or serves no state.
var archiveProbeCandidates = []uint64{1_000_000, 5_000_000, 10_000_000, 20_000_000, 50_000_000, 100_000_000, 500_000_000}

var errSearchPending = errors.New("first archive block search has not completed yet")

// EthFirstArchiveBlockNumber reports the earliest block whose state the node
// still retains. It mirrors the probe used by `proverator nodes info
// --check-archive`: binary search over eth_getBalance, which succeeds only at
// heights the node has not pruned.
//
// The search costs about log2(head) sequential state reads (~25 on mainnet), so
// it is far too expensive to run on every scrape. A background goroutine
// refreshes the value on a fixed interval instead, and Collect serves the last
// result.
type EthFirstArchiveBlockNumber struct {
	rpc  *rpc.Client
	desc *prometheus.Desc

	mu    sync.RWMutex
	block uint64
	err   error
}

// NewEthFirstArchiveBlockNumber starts a background refresh loop that keeps
// probing the node every interval, and never stops. That is fine because the
// exporter registers its collectors once and then runs until the process exits.
func NewEthFirstArchiveBlockNumber(rpc *rpc.Client, interval time.Duration) *EthFirstArchiveBlockNumber {
	collector := newEthFirstArchiveBlockNumber(rpc)

	go collector.refreshLoop(interval)

	return collector
}

// newEthFirstArchiveBlockNumber builds the collector without starting the
// refresh loop, so tests can drive refresh themselves.
func newEthFirstArchiveBlockNumber(rpc *rpc.Client) *EthFirstArchiveBlockNumber {
	return &EthFirstArchiveBlockNumber{
		rpc: rpc,
		desc: prometheus.NewDesc(
			"eth_first_archive_block_number",
			"earliest block whose state the node still retains",
			nil,
			nil,
		),
		err: errSearchPending,
	}
}

func (collector *EthFirstArchiveBlockNumber) Describe(ch chan<- *prometheus.Desc) {
	ch <- collector.desc
}

func (collector *EthFirstArchiveBlockNumber) Collect(ch chan<- prometheus.Metric) {
	collector.mu.RLock()
	block, err := collector.block, collector.err
	collector.mu.RUnlock()

	if err != nil {
		ch <- prometheus.NewInvalidMetric(collector.desc, err)
		return
	}

	ch <- prometheus.MustNewConstMetric(collector.desc, prometheus.GaugeValue, float64(block))
}

// refreshLoop probes once at startup, so the series does not stay absent until
// the first boundary, and then only on wall-clock boundaries.
func (collector *EthFirstArchiveBlockNumber) refreshLoop(interval time.Duration) {
	collector.refresh()

	for {
		time.Sleep(time.Until(nextTick(time.Now(), interval)))
		collector.refresh()
	}
}

// nextTick returns the next wall-clock boundary that is a whole multiple of
// interval — 01:00, 02:00, ... for the default hour — rather than a multiple of
// interval counted from process start. Truncate measures from the zero time in
// UTC, which is what makes the boundaries absolute, and identical across
// restarts and across every exporter instance.
func nextTick(now time.Time, interval time.Duration) time.Time {
	return now.Truncate(interval).Add(interval)
}

// refresh runs one search and stores the outcome. A failed search replaces the
// previous value with an error rather than serving a stale block number.
func (collector *EthFirstArchiveBlockNumber) refresh() {
	started := time.Now()

	// A missing head only widens the search, so an error here is not fatal.
	head, err := collector.headBlock()
	if err != nil {
		log.Printf("eth_first_archive_block_number: cannot read head block: %v", err)
	}

	block, probes, err := collector.findFirstArchiveBlock(head)
	if err != nil {
		log.Printf("eth_first_archive_block_number: search failed after %d probes: %v", probes, err)
	} else {
		log.Printf("eth_first_archive_block_number: %d (head %d, %d probes, %s)", block, head, probes, time.Since(started).Round(time.Millisecond))
	}

	collector.mu.Lock()
	collector.block, collector.err = block, err
	collector.mu.Unlock()
}

// findFirstArchiveBlock binary-searches the earliest block whose state the node
// still serves, and reports how many probes it took. head, when non-zero, seeds
// the upper bound. A probe that fails to reach the node aborts the search: half
// a search cannot be completed into a trustworthy answer.
func (collector *EthFirstArchiveBlockNumber) findFirstArchiveBlock(head uint64) (uint64, int, error) {
	probes := 0
	hasState := func(block uint64) (bool, error) {
		probes++

		state, err := collector.hasState(block)
		if err != nil {
			return false, fmt.Errorf("probing block %d: %w", block, err)
		}

		return state, nil
	}

	state, err := hasState(1)
	if err != nil {
		return 0, probes, err
	}
	if state {
		return 1, probes, nil
	}

	var high uint64
	if head > 0 {
		state, err := hasState(head)
		if err != nil {
			return 0, probes, err
		}
		if state {
			high = head
		}
	}
	if high == 0 {
		for _, candidate := range archiveProbeCandidates {
			state, err := hasState(candidate)
			if err != nil {
				return 0, probes, err
			}
			if state {
				high = candidate
				break
			}
		}
	}
	if high == 0 {
		if !collector.reachable() {
			return 0, probes, errors.New("node unreachable")
		}
		return 0, probes, errors.New("node serves no state at any probed height")
	}

	var low uint64
	for high-low > 1 {
		mid := (low + high) / 2

		state, err := hasState(mid)
		if err != nil {
			return 0, probes, err
		}
		if state {
			high = mid
		} else {
			low = mid
		}
	}

	return high, probes, nil
}

// hasState reports whether the node still serves state at block by asking for a
// balance at that height. Pruned state answers with a JSON-RPC error instead.
//
// Only an answer from the node itself counts as "no state here". A transport
// failure — connection reset, timeout, a proxy returning 5xx — says nothing
// about retention, so it is returned as an error and aborts the search. Reading
// one as the other would move the binary search past the real floor and report
// a wrong block number as if it were a clean result.
func (collector *EthFirstArchiveBlockNumber) hasState(block uint64) (bool, error) {
	var result json.RawMessage
	if err := collector.rpc.Call(&result, "eth_getBalance", zeroAddress, hexutil.EncodeUint64(block)); err != nil {
		var rpcErr rpc.Error
		if errors.As(err, &rpcErr) {
			return false, nil
		}

		return false, err
	}

	return len(result) > 0 && string(result) != "null", nil
}

// reachable distinguishes an unreachable node from one that simply serves no
// state, so a failed search says which of the two happened.
func (collector *EthFirstArchiveBlockNumber) reachable() bool {
	var version string
	return collector.rpc.Call(&version, "web3_clientVersion") == nil
}

func (collector *EthFirstArchiveBlockNumber) headBlock() (uint64, error) {
	var result hexutil.Uint64
	if err := collector.rpc.Call(&result, "eth_blockNumber"); err != nil {
		return 0, err
	}

	return uint64(result), nil
}
