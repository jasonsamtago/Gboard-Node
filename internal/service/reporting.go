package service

import (
	"fmt"

	"github.com/jasonsamtago/Gboard-Node/internal/controlplane"
	"github.com/jasonsamtago/Gboard-Node/internal/monitor"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// pushReportAsync admits one periodic report without blocking the service loop.
func (s *Service) pushReportAsync() {
	if s.reportStopping.Load() || !s.sink.SupportsReporting() {
		return
	}
	if !s.reportMu.TryLock() {
		nlog.Core().Debug("push already in progress, skipping")
		return
	}
	if s.reportStopping.Load() {
		s.reportMu.Unlock()
		return
	}
	if s.pushBackoff.shouldSkip() {
		nlog.Core().Debug("skipping report due to backoff")
		s.reportMu.Unlock()
		return
	}

	s.pushActive.Store(true)
	payload := s.reportSnapshot()
	go func() {
		defer func() {
			s.pushActive.Store(false)
			s.reportMu.Unlock()
		}()

		if err := s.sendReport(payload); err != nil {
			s.pushBackoff.onFailure()
			nlog.Core().Warn("failed to push report", "error", err)
			return
		}
		s.pushBackoff.onSuccess()
		nlog.ReportPushed(len(payload.Traffic), len(payload.Online))
	}()
}

// pushReportSync seals periodic admission and sends the final report once.
func (s *Service) pushReportSync() error {
	s.finalReportOnce.Do(func() {
		s.reportStopping.Store(true)
		s.reportMu.Lock()
		defer s.reportMu.Unlock()

		if !s.sink.SupportsReporting() {
			return
		}
		if err := s.sendReport(s.reportSnapshot()); err != nil {
			nlog.Core().Warn("failed to push final report", "error", err)
			s.finalReportErr = fmt.Errorf("push final report: %w", err)
		}
	})
	return s.finalReportErr
}

// reportSnapshot detaches all tracker data needed by one report.
// The caller holds reportMu.
func (s *Service) reportSnapshot() controlplane.ReportPayload {
	traffic := s.tracker.FlushTraffic()
	aliveIPs := s.tracker.FlushAliveIPs()
	online := s.tracker.CurrentOnline()
	status := monitor.Collect()
	metrics := s.buildMetrics(status)
	metrics["kernel_status"] = s.kernel.IsRunning()

	return controlplane.ReportPayload{
		Traffic: traffic,
		Alive:   aliveIPs,
		Online:  online,
		CPU:     status.CPU,
		Mem:     [2]uint64{status.MemTotal, status.MemUsed},
		Swap:    [2]uint64{status.SwapTotal, status.SwapUsed},
		Disk:    [2]uint64{status.DiskTotal, status.DiskUsed},
		Metrics: metrics,
	}
}

// sendReport delivers one detached report and restores in-memory state on failure.
// The caller holds reportMu.
func (s *Service) sendReport(payload controlplane.ReportPayload) error {
	if err := s.sink.Report(payload); err != nil {
		if len(payload.Traffic) > 0 {
			s.tracker.RestoreTraffic(payload.Traffic)
		}
		s.tracker.RestoreAliveIPs(payload.Alive)
		return err
	}
	return nil
}
