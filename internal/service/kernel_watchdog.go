package service

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

const defaultKernelWatchInterval = 15 * time.Second

func kernelWatchInterval(s *Service) time.Duration {
	if s != nil && s.kernelWatchInterval > 0 {
		return s.kernelWatchInterval
	}
	return defaultKernelWatchInterval
}

// WatchKernel is the in-process wrapper watchdog for official
// cedar2025/Xboard-Node #33. Container Up / IsRunning is not health:
// a dead singbox/Hy2 process or a missing UDP listen must restart the
// kernel here. A healthy kernel is left alone. This is not docker rm.
func (s *Service) WatchKernel() {
	if s == nil || s.kernel == nil || s.lastConfig == nil || len(s.lastUsers) == 0 {
		return
	}

	reason, unhealthy := s.kernelUnhealthy()
	if !unhealthy {
		s.watchdogLog(false, "watchdog: kernel healthy, skip")
		return
	}

	s.watchdogLog(true, fmt.Sprintf("watchdog: %s, restarting kernel", reason))
	if s.nodeLog != nil {
		nlog.FullRestart(s.nodeLog, reason)
	} else {
		nlog.Core().Info(fmt.Sprintf("kernel restart: %s", reason))
	}
	s.startKernel(s.lastConfig, s.lastUsers)
}

func (s *Service) kernelUnhealthy() (string, bool) {
	if !s.kernel.IsRunning() {
		return "kernel process exited", true
	}
	if s.lastConfig != nil && udpProtocol(s.lastConfig.Protocol) {
		port := s.lastConfig.ServerPort
		if port > 0 && !kernelUDPListenAlive(port) {
			return fmt.Sprintf("UDP listen missing on :%d", port), true
		}
	}
	return "", false
}

func (s *Service) watchdogLog(warn bool, msg string) {
	nl := s.nodeLog
	if nl == nil {
		nl = nlog.Core()
	}
	if warn {
		nl.Warn(msg)
		return
	}
	nl.Info(msg)
}

func udpProtocol(proto string) bool {
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "hysteria", "hysteria2", "hy2", "tuic":
		return true
	default:
		return false
	}
}

// kernelUDPListenAlive mirrors ss -ulnp port presence via /proc/net/udp.
func kernelUDPListenAlive(port int) bool {
	want := fmt.Sprintf(":%04X", port)
	for _, path := range []string{"/proc/net/udp", "/proc/net/udp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || !strings.Contains(fields[1], ":") {
				continue
			}
			if strings.HasSuffix(strings.ToUpper(fields[1]), want) {
				return true
			}
		}
	}
	return false
}
