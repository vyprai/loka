package supervisor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vyprai/loka/internal/metrics"
	"github.com/vyprai/loka/internal/worker/vm"
)

// MetricsCollector collects system and HTTP metrics inside the VM.
type MetricsCollector struct {
	mu     sync.RWMutex
	points []metrics.DataPoint
	probes []HTTPProbe

	// CPU tracking for delta calculation.
	prevCPUUser   uint64
	prevCPUSystem uint64
	prevCPUIdle   uint64
	prevCPUTotal  uint64

	// HTTP probe tracking.
	httpRequestCounts map[string]map[int]float64 // port:status_code -> count
}

// HTTPProbe defines an HTTP endpoint to probe.
type HTTPProbe struct {
	Port      int    `json:"port"`
	Path      string `json:"path"`
	Component string `json:"component"`
}

// MetricsConfig is received via set_metrics_config RPC.
type MetricsConfig struct {
	HTTPProbes []HTTPProbe `json:"http_probes"`
}

// NewMetricsCollector creates a new metrics collector.
func NewMetricsCollector() *MetricsCollector {
	mc := &MetricsCollector{
		httpRequestCounts: make(map[string]map[int]float64),
	}
	go mc.collectLoop()
	return mc
}

// SetConfig updates the metrics configuration (HTTP probes).
func (mc *MetricsCollector) SetConfig(cfg MetricsConfig) {
	mc.mu.Lock()
	mc.probes = cfg.HTTPProbes
	mc.mu.Unlock()
}

// GetPoints returns the latest cached metrics snapshot.
func (mc *MetricsCollector) GetPoints() []metrics.DataPoint {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	result := make([]metrics.DataPoint, len(mc.points))
	copy(result, mc.points)
	return result
}

func (mc *MetricsCollector) collectLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Collect immediately.
	mc.collect()

	for range ticker.C {
		mc.collect()
	}
}

func (mc *MetricsCollector) collect() {
	now := time.Now().UnixMilli()
	var points []metrics.DataPoint

	// CPU usage.
	if cpu := mc.collectCPU(); cpu >= 0 {
		points = append(points, metrics.DataPoint{
			Name: "vm_cpu_usage_percent", Type: metrics.Gauge,
			Timestamp: now, Value: cpu,
		})
	}

	// Memory.
	memUsed, memTotal := collectMemory()
	if memTotal > 0 {
		points = append(points, metrics.DataPoint{
			Name: "vm_memory_used_bytes", Type: metrics.Gauge,
			Timestamp: now, Value: float64(memUsed),
		})
		points = append(points, metrics.DataPoint{
			Name: "vm_memory_total_bytes", Type: metrics.Gauge,
			Timestamp: now, Value: float64(memTotal),
		})
	}

	// Disk.
	diskUsed, diskTotal := collectDisk()
	if diskTotal > 0 {
		points = append(points, metrics.DataPoint{
			Name: "vm_disk_used_bytes", Type: metrics.Gauge,
			Timestamp: now, Value: float64(diskUsed),
		})
		points = append(points, metrics.DataPoint{
			Name: "vm_disk_total_bytes", Type: metrics.Gauge,
			Timestamp: now, Value: float64(diskTotal),
		})
	}

	// Network.
	rxBytes, txBytes, rxPkts, txPkts := collectNetwork()
	points = append(points,
		metrics.DataPoint{Name: "vm_net_rx_bytes", Type: metrics.Counter, Timestamp: now, Value: float64(rxBytes)},
		metrics.DataPoint{Name: "vm_net_tx_bytes", Type: metrics.Counter, Timestamp: now, Value: float64(txBytes)},
		metrics.DataPoint{Name: "vm_net_rx_packets", Type: metrics.Counter, Timestamp: now, Value: float64(rxPkts)},
		metrics.DataPoint{Name: "vm_net_tx_packets", Type: metrics.Counter, Timestamp: now, Value: float64(txPkts)},
	)

	// Process count.
	if procCount := collectProcessCount(); procCount >= 0 {
		points = append(points, metrics.DataPoint{
			Name: "vm_process_count", Type: metrics.Gauge,
			Timestamp: now, Value: float64(procCount),
		})
	}

	// HTTP probes.
	mc.mu.RLock()
	probes := make([]HTTPProbe, len(mc.probes))
	copy(probes, mc.probes)
	mc.mu.RUnlock()

	for _, probe := range probes {
		httpPoints := mc.probeHTTP(probe, now)
		points = append(points, httpPoints...)
	}

	mc.mu.Lock()
	mc.points = points
	mc.mu.Unlock()
}

// collectCPU reads /proc/stat and computes CPU usage percent since last call.
func (mc *MetricsCollector) collectCPU() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return -1
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 {
		return -1
	}
	// First line: "cpu  user nice system idle iowait irq softirq steal"
	fields := strings.Fields(lines[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return -1
	}

	user, _ := strconv.ParseUint(fields[1], 10, 64)
	nice, _ := strconv.ParseUint(fields[2], 10, 64)
	system, _ := strconv.ParseUint(fields[3], 10, 64)
	idle, _ := strconv.ParseUint(fields[4], 10, 64)
	var iowait, irq, softirq, steal uint64
	if len(fields) > 5 {
		iowait, _ = strconv.ParseUint(fields[5], 10, 64)
	}
	if len(fields) > 6 {
		irq, _ = strconv.ParseUint(fields[6], 10, 64)
	}
	if len(fields) > 7 {
		softirq, _ = strconv.ParseUint(fields[7], 10, 64)
	}
	if len(fields) > 8 {
		steal, _ = strconv.ParseUint(fields[8], 10, 64)
	}

	totalUser := user + nice
	totalSystem := system + irq + softirq + steal
	totalIdle := idle + iowait
	total := totalUser + totalSystem + totalIdle

	if mc.prevCPUTotal == 0 {
		mc.prevCPUUser = totalUser
		mc.prevCPUSystem = totalSystem
		mc.prevCPUIdle = totalIdle
		mc.prevCPUTotal = total
		return -1 // Need two samples for delta.
	}

	deltaTotal := float64(total - mc.prevCPUTotal)
	deltaIdle := float64(totalIdle - mc.prevCPUIdle)

	mc.prevCPUUser = totalUser
	mc.prevCPUSystem = totalSystem
	mc.prevCPUIdle = totalIdle
	mc.prevCPUTotal = total

	if deltaTotal == 0 {
		return 0
	}
	return math.Round((1.0-deltaIdle/deltaTotal)*10000) / 100 // percent with 2 decimals
}

// collectMemory reads /proc/meminfo.
func collectMemory() (used, total uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var memTotal, memAvailable uint64
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			memTotal = parseMemInfoValue(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			memAvailable = parseMemInfoValue(line)
		}
	}
	if memTotal == 0 {
		return 0, 0
	}
	return (memTotal - memAvailable) * 1024, memTotal * 1024 // kB to bytes
}

func parseMemInfoValue(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v
}

// collectDisk uses syscall.Statfs on "/".
func collectDisk() (used, total uint64) {
	var stat syscallStatfs
	if err := statfs("/", &stat); err != nil {
		return 0, 0
	}
	total = stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	return total - free, total
}

// collectNetwork reads /proc/net/dev.
func collectNetwork() (rxBytes, txBytes, rxPkts, txPkts uint64) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.Contains(line, ":") || strings.HasPrefix(line, "lo:") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) < 2 {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 10 {
			continue
		}
		rb, _ := strconv.ParseUint(fields[0], 10, 64)
		rp, _ := strconv.ParseUint(fields[1], 10, 64)
		tb, _ := strconv.ParseUint(fields[8], 10, 64)
		tp, _ := strconv.ParseUint(fields[9], 10, 64)
		rxBytes += rb
		txBytes += tb
		rxPkts += rp
		txPkts += tp
	}
	return
}

// collectProcessCount counts entries in /proc/[0-9]+.
func collectProcessCount() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) > 0 && name[0] >= '0' && name[0] <= '9' {
			count++
		}
	}
	return count
}

// probeHTTP probes a single HTTP endpoint.
func (mc *MetricsCollector) probeHTTP(probe HTTPProbe, now int64) []metrics.DataPoint {
	var points []metrics.DataPoint
	var labels metrics.Labels
	labels = append(labels, metrics.Label{Name: "port", Value: strconv.Itoa(probe.Port)})
	if probe.Component != "" {
		labels = append(labels, metrics.Label{Name: "component", Value: probe.Component})
	}

	path := probe.Path
	if path == "" {
		path = "/"
	}

	addr := fmt.Sprintf("http://127.0.0.1:%d%s", probe.Port, path)

	// Check if port is even listening first (TCP connect).
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", probe.Port), 2*time.Second)
	if err != nil {
		points = append(points, metrics.DataPoint{
			Name: "vm_http_up", Type: metrics.Gauge,
			Labels: labels, Timestamp: now, Value: 0,
		})
		return points
	}
	conn.Close()

	// HTTP request.
	client := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	resp, err := client.Get(addr)
	duration := time.Since(start).Seconds()

	if err != nil {
		points = append(points, metrics.DataPoint{
			Name: "vm_http_up", Type: metrics.Gauge,
			Labels: labels, Timestamp: now, Value: 0,
		})
		return points
	}
	resp.Body.Close()

	points = append(points, metrics.DataPoint{
		Name: "vm_http_up", Type: metrics.Gauge,
		Labels: labels, Timestamp: now, Value: 1,
	})

	// Latency.
	points = append(points, metrics.DataPoint{
		Name: "vm_http_latency_seconds", Type: metrics.Gauge,
		Labels: labels, Timestamp: now, Value: duration,
	})

	// Request count by status code.
	statusLabels := make(metrics.Labels, len(labels)+1)
	copy(statusLabels, labels)
	statusLabels = append(statusLabels, metrics.Label{Name: "status_code", Value: strconv.Itoa(resp.StatusCode)})
	statusLabels.Sort()

	key := fmt.Sprintf("%d", probe.Port)
	mc.mu.Lock()
	if mc.httpRequestCounts[key] == nil {
		mc.httpRequestCounts[key] = make(map[int]float64)
	}
	mc.httpRequestCounts[key][resp.StatusCode]++
	count := mc.httpRequestCounts[key][resp.StatusCode]
	mc.mu.Unlock()

	points = append(points, metrics.DataPoint{
		Name: "vm_http_requests_total", Type: metrics.Counter,
		Labels: statusLabels, Timestamp: now, Value: count,
	})

	return points
}

// handleMetricsScrape handles the "metrics_scrape" RPC method.
func (s *Server) handleMetricsScrape(req vm.RPCRequest) vm.RPCResponse {
	if s.metricsCollector == nil {
		return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`{"points":[]}`)}
	}
	points := s.metricsCollector.GetPoints()
	result, _ := json.Marshal(map[string]interface{}{"points": points})
	return vm.RPCResponse{ID: req.ID, Result: result}
}

// handleSetMetricsConfig handles the "set_metrics_config" RPC method.
func (s *Server) handleSetMetricsConfig(req vm.RPCRequest) vm.RPCResponse {
	var cfg MetricsConfig
	if err := json.Unmarshal(req.Params, &cfg); err != nil {
		return rpcError(req.ID, fmt.Errorf("invalid metrics config: %w", err))
	}
	if s.metricsCollector == nil {
		s.metricsCollector = NewMetricsCollector()
	}
	s.metricsCollector.SetConfig(cfg)
	return vm.RPCResponse{ID: req.ID, Result: jsonRaw(`"ok"`)}
}

// syscallStatfs holds filesystem stats.
type syscallStatfs struct {
	Blocks uint64
	Bfree  uint64
	Bsize  int64
}
