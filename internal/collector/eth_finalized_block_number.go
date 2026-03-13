package collector

import (
	"log/slog"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/prometheus/client_golang/prometheus"
)

type EthFinalizedBlockNumber struct {
	rpc  *rpc.Client
	url  string
	desc *prometheus.Desc
}

func NewEthFinalizedBlockNumber(rpc *rpc.Client, url string) *EthFinalizedBlockNumber {
	return &EthFinalizedBlockNumber{
		rpc: rpc,
		url: url,
		desc: prometheus.NewDesc(
			"eth_finalized_block_number",
			"Number of the most recent finalized block",
			nil,
			nil,
		),
	}
}

func (collector *EthFinalizedBlockNumber) Describe(ch chan<- *prometheus.Desc) {
	ch <- collector.desc
}

func (collector *EthFinalizedBlockNumber) Collect(ch chan<- prometheus.Metric) {
	var result struct {
		Number hexutil.Uint64 `json:"number"`
	}

	if err := collector.rpc.Call(&result, "eth_getBlockByNumber", "finalized", false); err != nil {
		slog.Error("collect failed", "metric", "eth_finalized_block_number", "url", collector.url, "error", err)
		ch <- prometheus.NewInvalidMetric(collector.desc, err)
		return
	}

	ch <- prometheus.MustNewConstMetric(collector.desc, prometheus.GaugeValue, float64(result.Number))
}
