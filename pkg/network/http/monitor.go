// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:build linux_bpf
// +build linux_bpf

package http

import (
	"fmt"
	"sync"

	"github.com/cilium/ebpf"

	manager "github.com/DataDog/ebpf-manager"

	ddebpf "github.com/DataDog/datadog-agent/pkg/ebpf"
	"github.com/DataDog/datadog-agent/pkg/network/config"
	filterpkg "github.com/DataDog/datadog-agent/pkg/network/filter"
	errtelemetry "github.com/DataDog/datadog-agent/pkg/network/telemetry"
)

// MonitorStats is used for holding two kinds of stats:
// * requestsStats which are the http data stats
// * telemetry which are telemetry stats
type MonitorStats struct {
	requestStats map[Key]*RequestStats
	telemetry    telemetry
}

// Monitor is responsible for:
// * Creating a raw socket and attaching an eBPF filter to it;
// * Polling a perf buffer that contains notifications about HTTP transaction batches ready to be read;
// * Querying these batches by doing a map lookup;
// * Aggregating and emitting metrics based on the received HTTP transactions;
type Monitor struct {
	handler func([]httpTX)

	ebpfProgram            *ebpfProgram
	batchManager           *batchManager
	batchCompletionHandler *ddebpf.PerfHandler
	telemetry              *telemetry
	telemetrySnapshot      *telemetry
	pollRequests           chan chan MonitorStats
	statkeeper             *httpStatKeeper

	// termination
	mux           sync.Mutex
	eventLoopWG   sync.WaitGroup
	closeFilterFn func()
	stopped       bool
}

// NewMonitor returns a new Monitor instance
func NewMonitor(c *config.Config, offsets []manager.ConstantEditor, sockFD *ebpf.Map, bpfTelemetry *errtelemetry.EBPFTelemetry) (*Monitor, error) {
	mgr, err := newEBPFProgram(c, offsets, sockFD, bpfTelemetry)
	if err != nil {
		return nil, fmt.Errorf("error setting up http ebpf program: %s", err)
	}

	if err := mgr.Init(); err != nil {
		return nil, fmt.Errorf("error initializing http ebpf program: %s", err)
	}

	filter, _ := mgr.GetProbe(manager.ProbeIdentificationPair{EBPFSection: httpSocketFilterStub, EBPFFuncName: "socket__http_filter_entry", UID: probeUID})
	if filter == nil {
		return nil, fmt.Errorf("error retrieving socket filter")
	}

	closeFilterFn, err := filterpkg.HeadlessSocketFilter(c, filter)
	if err != nil {
		return nil, fmt.Errorf("error enabling HTTP traffic inspection: %s", err)
	}

	batchMap, _, err := mgr.GetMap(httpBatchesMap)
	if err != nil {
		return nil, err
	}

	batchEventsMap, _, _ := mgr.GetMap(httpBatchEvents)
	numCPUs := int(batchEventsMap.MaxEntries())

	telemetry, err := newTelemetry()
	if err != nil {
		return nil, err
	}
	statkeeper := newHTTPStatkeeper(c, telemetry)

	handler := func(transactions []httpTX) {
		if statkeeper != nil {
			statkeeper.Process(transactions)
		}
	}

	batchManager, err := newBatchManager(batchMap, numCPUs)
	if err != nil {
		return nil, fmt.Errorf("couldn't instantiate batch manager: %w", err)
	}

	return &Monitor{
		handler:                handler,
		ebpfProgram:            mgr,
		batchManager:           batchManager,
		batchCompletionHandler: mgr.batchCompletionHandler,
		telemetry:              telemetry,
		telemetrySnapshot:      nil,
		pollRequests:           make(chan chan MonitorStats),
		closeFilterFn:          closeFilterFn,
		statkeeper:             statkeeper,
	}, nil
}

// Start consuming HTTP events
func (m *Monitor) Start() error {
	if m == nil {
		return nil
	}

	if err := m.ebpfProgram.Start(); err != nil {
		return err
	}

	m.eventLoopWG.Add(1)
	go func() {
		defer m.eventLoopWG.Done()
		for {
			select {
			case dataEvent, ok := <-m.batchCompletionHandler.DataChannel:
				if !ok {
					return
				}

				transactions, err := m.batchManager.GetTransactionsFrom(dataEvent)
				m.process(transactions, err)
				dataEvent.Done()
			case _, ok := <-m.batchCompletionHandler.LostChannel:
				if !ok {
					return
				}

				m.process(nil, errLostBatch)
			case reply, ok := <-m.pollRequests:
				if !ok {
					return
				}

				transactions := m.batchManager.GetPendingTransactions()
				m.process(transactions, nil)

				delta := m.telemetry.reset()

				// For now, we still want to report the telemetry as it contains more information than what
				// we're extracting via network tracer.
				delta.report()

				reply <- MonitorStats{
					requestStats: m.statkeeper.GetAndResetAllStats(),
					telemetry:    delta,
				}
			}
		}
	}()

	return nil
}

// GetHTTPStats returns a map of HTTP stats stored in the following format:
// [source, dest tuple, request path] -> RequestStats object
func (m *Monitor) GetHTTPStats() map[Key]*RequestStats {
	if m == nil {
		return nil
	}

	m.mux.Lock()
	defer m.mux.Unlock()
	if m.stopped {
		return nil
	}

	reply := make(chan MonitorStats, 1)
	defer close(reply)
	m.pollRequests <- reply
	stats := <-reply
	m.telemetrySnapshot = &stats.telemetry
	return stats.requestStats
}

// GetStats returns the telemetry
func (m *Monitor) GetStats() map[string]interface{} {
	empty := map[string]interface{}{}
	if m == nil {
		return empty
	}

	m.mux.Lock()
	defer m.mux.Unlock()
	if m.stopped {
		return empty
	}

	if m.telemetrySnapshot == nil {
		return empty
	}

	return m.telemetrySnapshot.report()
}

// Stop HTTP monitoring
func (m *Monitor) Stop() {
	if m == nil {
		return
	}

	m.mux.Lock()
	defer m.mux.Unlock()
	if m.stopped {
		return
	}

	m.ebpfProgram.Close()
	m.closeFilterFn()
	close(m.pollRequests)
	m.eventLoopWG.Wait()
	m.stopped = true
}

func (m *Monitor) process(transactions []httpTX, err error) {
	m.telemetry.aggregate(transactions, err)

	if m.handler != nil && len(transactions) > 0 {
		m.handler(transactions)
	}
}

// DumpMaps dumps the maps associated with the monitor
func (m *Monitor) DumpMaps(maps ...string) (string, error) {
	return m.ebpfProgram.DumpMaps(maps...)
}
