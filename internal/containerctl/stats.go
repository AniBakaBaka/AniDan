// SPDX-License-Identifier: AGPL-3.0-only
package containerctl

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

type CPUStats struct {
	Usage struct {
		Total  uint64   `json:"total_usage"`
		PerCPU []uint64 `json:"percpu_usage"`
	} `json:"cpu_usage"`
	System uint64 `json:"system_cpu_usage"`
	Online int    `json:"online_cpus"`
}
type Stats struct {
	Read        time.Time `json:"read"`
	CPU         CPUStats  `json:"cpu_stats"`
	PreviousCPU CPUStats  `json:"precpu_stats"`
	Memory      struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RX uint64 `json:"rx_bytes"`
		TX uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlockIO struct {
		Bytes []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

func (c *Controller) Sample(ctx context.Context, previous *Stats) (map[string]any, Stats, error) {
	info, err := c.inspect(ctx)
	if err != nil {
		return nil, Stats{}, err
	}
	stats, err := c.engine.Stats(ctx, info.ID)
	if err != nil {
		return nil, Stats{}, err
	}
	return FormatStats(info, stats, previous), stats, nil
}
func delta(now, before uint64) float64 {
	if now < before {
		return 0
	}
	return float64(now - before)
}
func round(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}
func byteString(v uint64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB", "EB"}
	x := float64(v)
	i := 0
	for x >= 1024 && i < len(units)-1 {
		x /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", x, units[i])
}
func networkTotals(s Stats) (uint64, uint64) {
	var rx, tx uint64
	for _, v := range s.Networks {
		rx += v.RX
		tx += v.TX
	}
	return rx, tx
}

// FormatStats exposes only resource measurements, never raw inspect/config data.
func FormatStats(info Inspection, s Stats, previous *Stats) map[string]any {
	before := s.PreviousCPU
	if previous != nil {
		before = previous.CPU
	}
	online := s.CPU.Online
	if online == 0 {
		online = len(s.CPU.Usage.PerCPU)
	}
	if online == 0 {
		online = 1
	}
	cpu := 0.0
	systemDelta := delta(s.CPU.System, before.System)
	if systemDelta > 0 {
		cpu = delta(s.CPU.Usage.Total, before.Usage.Total) / systemDelta * float64(online) * 100
	}
	cache := s.Memory.Stats["total_inactive_file"]
	if cache == 0 {
		cache = s.Memory.Stats["inactive_file"]
	}
	if cache == 0 {
		cache = s.Memory.Stats["cache"]
	}
	used := s.Memory.Usage
	if cache < used {
		used -= cache
	} else {
		used = 0
	}
	mem := 0.0
	if s.Memory.Limit > 0 {
		mem = float64(used) / float64(s.Memory.Limit) * 100
	}
	rx, tx := networkTotals(s)
	rxRate, txRate := 0.0, 0.0
	if previous != nil {
		seconds := s.Read.Sub(previous.Read).Seconds()
		if seconds > 0 {
			prx, ptx := networkTotals(*previous)
			rxRate = delta(rx, prx) / seconds
			txRate = delta(tx, ptx) / seconds
		}
	}
	var read, write uint64
	for _, v := range s.BlockIO.Bytes {
		switch strings.ToLower(v.Op) {
		case "read":
			read += v.Value
		case "write":
			write += v.Value
		}
	}
	return map[string]any{"available": true, "containerName": strings.TrimPrefix(info.Name, "/"), "containerId": info.ID, "status": info.State.Status, "startedAt": info.State.StartedAt,
		"cpu":     map[string]any{"percent": round(cpu), "onlineCpus": online},
		"memory":  map[string]any{"usage": used, "limit": s.Memory.Limit, "percent": round(mem), "usageFormatted": byteString(used), "limitFormatted": byteString(s.Memory.Limit)},
		"network": map[string]any{"rxBytes": rx, "txBytes": tx, "rxRate": round(rxRate), "txRate": round(txRate), "rxFormatted": byteString(rx), "txFormatted": byteString(tx), "rxRateFormatted": byteString(uint64(rxRate)) + "/s", "txRateFormatted": byteString(uint64(txRate)) + "/s"},
		"io":      map[string]any{"readBytes": read, "writeBytes": write, "readFormatted": byteString(read), "writeFormatted": byteString(write)}}
}
