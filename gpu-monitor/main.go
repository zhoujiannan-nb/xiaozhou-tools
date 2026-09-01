// gpu-monitor: Windows 下长时间(默认 6h)监控每进程 GPU 占用 + 内嵌 Web 看板
//
// 数据源(自动探测, 与任务管理器同源):
//   1. WMI Raw 类  Win32_PerfRawData_GPUPerformanceCounters_GPUEngine   (Win11 24H2: 每进程+每引擎, 差值法)
//   2. WMI 格式化类 Win32_PerfFormattedData_..._GPUProcessUtilization    (Win10: 每进程, 直接百分比)
//   3. WMI Raw 类  Win32_PerfRawData_GPUPerformanceCounters_GPUProcess  (Win10 兜底, 差值法)
// 显存: Win32_PerfRawData_GPUPerformanceCounters_GPUProcessMemory (尽力而为)
//
// 用法:
//   gpu-monitor.exe                    # 监控 6 小时, Web 开在 :7777
//   gpu-monitor.exe -selftest          # 自检: 验证数据源是否可用
//   gpu-monitor.exe -duration 1h -interval 2s -csv D:\data\gpu.csv
package main

import (
	"embed"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/StackExchange/wmi"
)

//go:embed web
var webFS embed.FS

// ---------- WMI 数据结构 ----------

// Raw 类: 属性名 = 计数器名去空格, 值为累计 100ns tick
type rawEngineRow struct {
	Name                  string `wmi:"Name"`
	UtilizationPercentage uint64 `wmi:"UtilizationPercentage"`
}

// Win10 格式化类: 直接百分比
type fmtProcRow struct {
	Name                  string `wmi:"Name"`
	ProcessId             uint32 `wmi:"ProcessId"`
	ProcessName           string `wmi:"ProcessName"`
	UtilizationPercentage uint32 `wmi:"UtilizationPercentage"`
}

type fmtInfoRow struct {
	Name                  string `wmi:"Name"`
	UtilizationPercentage uint32 `wmi:"UtilizationPercentage"`
}

type vramRow struct {
	Name           string `wmi:"Name"`
	DedicatedUsage uint64 `wmi:"DedicatedUsage"`
	SharedUsage    uint64 `wmi:"SharedUsage"`
}

type Win32Process struct {
	ProcessId      uint32  `wmi:"ProcessId"`
	Name           string  `wmi:"Name"`
	ExecutablePath *string `wmi:"ExecutablePath"`
}

type Win32VideoController struct {
	Name string `wmi:"Name"`
}

const (
	qRawEngine = `SELECT * FROM Win32_PerfRawData_GPUPerformanceCounters_GPUEngine`
	qFmtProc   = `SELECT * FROM Win32_PerfFormattedData_GPUPerformanceCounters_GPUProcessUtilization`
	qRawProc   = `SELECT * FROM Win32_PerfRawData_GPUPerformanceCounters_GPUProcess`
	qFmtInfo   = `SELECT * FROM Win32_PerfFormattedData_GPUPerformanceCounters_GPUProcessorInfo`
	qVRAM      = `SELECT * FROM Win32_PerfRawData_GPUPerformanceCounters_GPUProcessMemory`
	qProcess   = `SELECT * FROM Win32_Process`
	qVideoCtrl = `SELECT * FROM Win32_VideoController`
)

// ---------- 实例名解析 ----------
// Win11 24H2: pid_1234_luid_0x00000000_0x00012710_phys_0_eng_0_engtype_3D
// Win10:      GPU 0_NVIDIA GeForce RTX 3070 Ti_PID 12345

var (
	rePid24h2  = regexp.MustCompile(`^pid_(\d+)_luid_(0x[0-9a-fA-F]+_0x[0-9a-fA-F]+)`)
	rePidWin10 = regexp.MustCompile(`_PID (\d+)$`)
)

func parseInstance(name string) (pid uint32, luid string, ok bool) {
	if m := rePid24h2.FindStringSubmatch(name); m != nil {
		v, _ := strconv.Atoi(m[1])
		return uint32(v), m[2], true
	}
	if m := rePidWin10.FindStringSubmatch(name); m != nil {
		v, _ := strconv.Atoi(m[1])
		return uint32(v), "", true
	}
	return 0, "", false
}

// ---------- 数据源 ----------

type procPct struct {
	Adapter string  // luid 或 ""(单卡)
	PID     uint32
	Pct     float64
}

type UtilSource interface {
	Collect() (procs []procPct, totals map[string]float64, err error)
	Name() string
}

// rawDeltaSource: Raw 类差值法 (24H2 GPUEngine / Win10 GPUProcess)
type rawDeltaSource struct {
	query    string
	name     string
	interval time.Duration
	prev     map[string]uint64
	ready    bool
}

func (s *rawDeltaSource) Name() string { return s.name }

func (s *rawDeltaSource) Collect() ([]procPct, map[string]float64, error) {
	var rows []rawEngineRow
	if err := wmi.Query(s.query, &rows); err != nil {
		return nil, nil, err
	}
	now := make(map[string]uint64, len(rows))
	for _, r := range rows {
		now[r.Name] = r.UtilizationPercentage
	}

	totals := map[string]float64{}
	var procs []procPct
	if s.ready {
		ticks := float64(s.interval.Seconds()) * 1e7 // 100ns tick
		perProc := map[string]float64{} // "pid|adapter" -> max pct
		procAd := map[string]string{}
		procPids := map[string]uint32{}
		for name, cur := range now {
			prev, ok := s.prev[name]
			if !ok || cur <= prev { // 新实例或计数器重置: 跳过
				continue
			}
			pct := float64(cur-prev) / ticks * 100
			if pct > 100 {
				pct = 100
			}
			pid, luid, ok2 := parseInstance(name)
			if !ok2 {
				continue
			}
			if pct > totals[luid] {
				totals[luid] = pct
			}
			key := strconv.FormatUint(uint64(pid), 10) + "|" + luid
			if pct > perProc[key] {
				perProc[key] = pct
				procAd[key] = luid
				procPids[key] = pid
			}
		}
		for key, pct := range perProc {
			procs = append(procs, procPct{Adapter: procAd[key], PID: procPids[key], Pct: pct})
		}
	}
	s.prev = now
	s.ready = true
	return procs, totals, nil
}

// fmtSource: Win10 格式化类, 直接百分比
type fmtSource struct{}

func (s *fmtSource) Name() string { return "WMI 格式化 GPUProcessUtilization (Win10)" }

func (s *fmtSource) Collect() ([]procPct, map[string]float64, error) {
	var rows []fmtProcRow
	if err := wmi.Query(qFmtProc, &rows); err != nil {
		return nil, nil, err
	}
	procs := make([]procPct, 0, len(rows))
	totals := map[string]float64{}
	for _, r := range rows {
		p := float64(r.UtilizationPercentage)
		procs = append(procs, procPct{PID: r.ProcessId, Pct: p})
		if p > totals[""] {
			totals[""] = p
		}
	}
	// 总占用: 尝试 GPUProcessorInfo
	var infos []fmtInfoRow
	if err := wmi.Query(qFmtInfo, &infos); err == nil {
		for _, i := range infos {
			if float64(i.UtilizationPercentage) > totals[""] {
				totals[""] = float64(i.UtilizationPercentage)
			}
		}
	}
	return procs, totals, nil
}

func detectSource(interval time.Duration) (UtilSource, error) {
	// 1) Raw GPUEngine (24H2)
	var a []rawEngineRow
	if err := wmi.Query(qRawEngine, &a); err == nil && len(a) > 0 {
		for _, r := range a {
			if _, _, ok := parseInstance(r.Name); ok {
				return &rawDeltaSource{query: qRawEngine, name: "WMI Raw GPUEngine (Win11 24H2)", interval: interval, prev: map[string]uint64{}}, nil
			}
		}
	}
	// 2) 格式化 GPUProcessUtilization (Win10)
	var b []fmtProcRow
	if err := wmi.Query(qFmtProc, &b); err == nil {
		return &fmtSource{}, nil
	}
	// 3) Raw GPUProcess (Win10 兜底)
	var c []rawEngineRow
	if err := wmi.Query(qRawProc, &c); err == nil && len(c) > 0 {
		for _, r := range c {
			if _, _, ok := parseInstance(r.Name); ok {
				return &rawDeltaSource{query: qRawProc, name: "WMI Raw GPUProcess (Win10)", interval: interval, prev: map[string]uint64{}}, nil
			}
		}
	}
	return nil, fmt.Errorf("未找到可用的 GPU 利用率数据源 (Raw/格式化 WMI 类均不存在, 请确认系统为 Win10+ 且已安装显卡驱动)")
}

// ---------- 内存数据 ----------

type procSample struct {
	Adapter string  `json:"adapter"`
	PID     uint32  `json:"pid"`
	Name    string  `json:"name"`
	Path    string  `json:"path"`
	Pct     float64 `json:"pct"`
	VRAM    uint64  `json:"vram"` // bytes
}

type sample struct {
	T      time.Time
	Totals map[string]float64
	Procs  []procSample
}

type procInfo struct {
	Name string
	Path string
}

type Server struct {
	mu         sync.RWMutex
	samples    []sample
	running    bool
	startedAt  time.Time
	finishedAt time.Time
	duration   time.Duration
	interval   time.Duration
	adapters   map[string]bool
	adapterNames map[string]string
	videoNames []string
	lastErr    string
	sourceName string

	pathCache   map[uint32]procInfo
	pathCacheAt time.Time

	src  UtilSource
	vram map[uint32]uint64

	csvF    *os.File
	csvW    *csv.Writer
	csvPath string

	stopCh   chan struct{}
	stopOnce sync.Once
}

func (s *Server) setErr(msg string) {
	s.mu.Lock()
	s.lastErr = msg
	s.mu.Unlock()
}

func (s *Server) noteAdapter(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adapters[key] {
		return
	}
	s.adapters[key] = true
	idx := len(s.adapters) - 1
	short := key
	if i := strings.LastIndex(key, "_0x"); i >= 0 {
		short = key[i+1:]
	}
	switch {
	case len(s.videoNames) == 1:
		if idx == 0 || key == "" {
			s.adapterNames[key] = s.videoNames[0]
		} else {
			s.adapterNames[key] = s.videoNames[0] + " (" + short + ")"
		}
	case len(s.videoNames) > 1 && idx < len(s.videoNames):
		s.adapterNames[key] = s.videoNames[idx]
	default:
		s.adapterNames[key] = fmt.Sprintf("GPU %d", idx)
		if key != "" {
			s.adapterNames[key] += " (" + short + ")"
		}
	}
}

func (s *Server) refreshPaths() {
	s.mu.Lock()
	fresh := time.Since(s.pathCacheAt) < 60*time.Second
	s.mu.Unlock()
	if fresh {
		return
	}
	var procs []Win32Process
	if err := wmi.Query(qProcess, &procs); err != nil {
		return
	}
	m := make(map[uint32]procInfo, len(procs))
	for _, p := range procs {
		path := ""
		if p.ExecutablePath != nil {
			path = *p.ExecutablePath
		}
		m[p.ProcessId] = procInfo{Name: p.Name, Path: path}
	}
	s.mu.Lock()
	s.pathCache = m
	s.pathCacheAt = time.Now()
	s.mu.Unlock()
}

func (s *Server) procInfo(pid uint32) procInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pathCache[pid]
}

// primaryAdapterLocked 返回累计总占用最高的显卡(调用方需持有读锁)
func (s *Server) primaryAdapterLocked() string {
	sums := map[string]float64{}
	for _, sm := range s.samples {
		for a, v := range sm.Totals {
			sums[a] += v
		}
	}
	best := ""
	var bestV float64
	for a, v := range sums {
		if v > bestV {
			bestV, best = v, a
		}
	}
	return best
}

// ---------- 采集 ----------

func (s *Server) collectLoop(interval, duration time.Duration) {
	// 预热一次, 让差值法有基线
	s.src.Collect()

	tick := time.NewTicker(interval)
	defer tick.Stop()
	var end time.Time
	if duration > 0 {
		end = time.Now().Add(duration)
	}
	for {
		select {
		case <-s.stopCh:
			s.finish()
			return
		case now := <-tick.C:
			s.collectOnce()
			if !end.IsZero() && now.After(end) {
				s.finish()
				return
			}
		}
	}
}

func (s *Server) finish() {
	s.mu.Lock()
	s.running = false
	s.finishedAt = time.Now()
	s.mu.Unlock()
	log.Println("监控结束")
}

func (s *Server) collectVRAM() map[uint32]uint64 {
	var rows []vramRow
	if err := wmi.Query(qVRAM, &rows); err != nil {
		return nil
	}
	m := make(map[uint32]uint64, len(rows))
	for _, r := range rows {
		pid, _, ok := parseInstance(r.Name)
		if !ok {
			continue
		}
		m[pid] += r.DedicatedUsage + r.SharedUsage
	}
	return m
}

func (s *Server) collectOnce() {
	procs, totals, err := s.src.Collect()
	if err != nil {
		s.setErr("数据源查询失败: " + err.Error())
		return
	}
	s.setErr("")

	now := time.Now()
	s.refreshPaths()
	vram := s.collectVRAM()

	sm := sample{T: now, Totals: map[string]float64{}}
	for a, v := range totals {
		sm.Totals[a] = v
		s.noteAdapter(a)
	}
	for _, p := range procs {
		info := s.procInfo(p.PID)
		name := info.Name
		if name == "" {
			name = "PID " + strconv.Itoa(int(p.PID))
		}
		sm.Procs = append(sm.Procs, procSample{
			Adapter: p.Adapter,
			PID:     p.PID,
			Name:    name,
			Path:    info.Path,
			Pct:     p.Pct,
			VRAM:    vram[p.PID],
		})
	}

	s.mu.Lock()
	s.samples = append(s.samples, sm)
	s.mu.Unlock()

	s.writeCSV(sm)
}

func (s *Server) writeCSV(sm sample) {
	ts := sm.T.Format("2006-01-02 15:04:05")
	if len(sm.Procs) == 0 {
		adapter, total := "", 0.0
		for a, v := range sm.Totals {
			if adapter == "" {
				adapter, total = a, v
			}
		}
		s.csvW.Write([]string{ts, adapter, strconv.FormatFloat(total, 'f', 1, 64), "", "", "", "", ""})
	} else {
		for _, p := range sm.Procs {
			s.csvW.Write([]string{
				ts, p.Adapter, strconv.FormatFloat(sm.Totals[p.Adapter], 'f', 1, 64),
				strconv.Itoa(int(p.PID)), p.Name, p.Path,
				strconv.FormatFloat(p.Pct, 'f', 1, 64),
				strconv.FormatFloat(float64(p.VRAM)/(1024*1024), 'f', 1, 64),
			})
		}
	}
	s.csvW.Flush()
}

// ---------- HTTP API ----------

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	running := s.running
	started := s.startedAt
	finished := s.finishedAt
	duration := s.duration
	errMsg := s.lastErr
	sourceName := s.sourceName
	adapters := make([]string, 0, len(s.adapters))
	for a := range s.adapters {
		adapters = append(adapters, s.adapterNames[a])
	}
	sort.Strings(adapters)
	var current map[string]interface{}
	if n := len(s.samples); n > 0 {
		sm := s.samples[n-1]
		procs := make([]map[string]interface{}, 0, len(sm.Procs))
		for _, p := range sm.Procs {
			procs = append(procs, map[string]interface{}{
				"adapter": s.adapterNames[p.Adapter], "pid": p.PID, "name": p.Name,
				"path": p.Path, "pct": p.Pct, "vram": p.VRAM,
			})
		}
		totals := map[string]float64{}
		for a, v := range sm.Totals {
			totals[s.adapterNames[a]] = v
		}
		current = map[string]interface{}{"totals": totals, "procs": procs}
	}
	s.mu.RUnlock()

	end := time.Now()
	if !finished.IsZero() {
		end = finished
	}
	elapsed := int64(end.Sub(started).Seconds())
	remaining := int64(0)
	if running && duration > 0 {
		remaining = int64(started.Add(duration).Sub(time.Now()).Seconds())
		if remaining < 0 {
			remaining = 0
		}
	}
	writeJSON(w, map[string]interface{}{
		"running":     running,
		"started":     started.UnixMilli(),
		"finished":    finished.UnixMilli(),
		"elapsed":     elapsed,
		"remaining":   remaining,
		"error":       errMsg,
		"source":      sourceName,
		"adapters":    adapters,
		"current":     current,
		"sampleCount": len(s.samples),
	})
}

type histOut struct {
	T     int64             `json:"t"`
	Total float64           `json:"total"`
	Procs map[string]float64 `json:"procs"`
}

func parseRange(s string) (time.Duration, bool) {
	if s == "all" {
		return 365 * 24 * time.Hour, true
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d, true
}

func downsample(pts []histOut, maxN int) []histOut {
	if len(pts) <= maxN {
		return pts
	}
	bucket := (len(pts) + maxN - 1) / maxN
	out := make([]histOut, 0, maxN)
	for i := 0; i < len(pts); i += bucket {
		end := i + bucket
		if end > len(pts) {
			end = len(pts)
		}
		p := histOut{T: pts[i].T, Procs: map[string]float64{}}
		for _, q := range pts[i:end] {
			if q.Total > p.Total {
				p.Total = q.Total
			}
			for k, v := range q.Procs {
				if v > p.Procs[k] {
					p.Procs[k] = v
				}
			}
		}
		out = append(out, p)
	}
	return out
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	adapter := q.Get("adapter")
	rangeStr := q.Get("range")
	if rangeStr == "" {
		rangeStr = "30m"
	}
	dur, ok := parseRange(rangeStr)
	if !ok {
		http.Error(w, "bad range", 400)
		return
	}

	s.mu.RLock()
	if adapter != "" {
		// 把显示名映射回内部 key
		for k, v := range s.adapterNames {
			if v == adapter {
				adapter = k
				break
			}
		}
	} else {
		adapter = s.primaryAdapterLocked()
	}
	cutoff := time.Now().Add(-dur)
	n := len(s.samples)
	idx := sort.Search(n, func(i int) bool { return !s.samples[i].T.Before(cutoff) })
	pts := make([]histOut, 0, n-idx)
	for _, sm := range s.samples[idx:] {
		p := histOut{T: sm.T.UnixMilli(), Procs: map[string]float64{}}
		if adapter == "" {
			for _, v := range sm.Totals {
				if v > p.Total {
					p.Total = v
				}
			}
		} else {
			p.Total = sm.Totals[adapter]
		}
		for _, pr := range sm.Procs {
			if adapter != "" && pr.Adapter != adapter {
				continue
			}
			if pr.Pct > p.Procs[pr.Name] {
				p.Procs[pr.Name] = pr.Pct
			}
		}
		pts = append(pts, p)
	}
	displayAdapter := s.adapterNames[adapter]
	s.mu.RUnlock()

	pts = downsample(pts, 600)
	writeJSON(w, map[string]interface{}{"adapter": displayAdapter, "points": pts})
}

type sumItem struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	TimeAbove  int64    `json:"timeAbove"`
	TimeActive int64    `json:"timeActive"`
	Max        float64  `json:"max"`
	Avg        float64  `json:"avg"`
	FirstSeen  int64    `json:"firstSeen"`
	LastSeen   int64    `json:"lastSeen"`
	PIDs       []uint32 `json:"pids"`
}

type sumAgg struct {
	path       string
	timeAbove  int64
	timeActive int64
	max        float64
	sum        float64
	activeN    int64
	first      time.Time
	last       time.Time
	pids       map[uint32]bool
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	adapter := q.Get("adapter")
	th, _ := strconv.Atoi(q.Get("threshold"))
	if th <= 0 {
		th = 10
	}

	s.mu.RLock()
	if adapter != "" {
		for k, v := range s.adapterNames {
			if v == adapter {
				adapter = k
				break
			}
		}
	} else {
		adapter = s.primaryAdapterLocked()
	}
	sec := int64(s.interval.Seconds())
	aggs := map[string]*sumAgg{}
	for _, sm := range s.samples {
		for _, pr := range sm.Procs {
			if adapter != "" && pr.Adapter != adapter {
				continue
			}
			a, ok := aggs[pr.Name]
			if !ok {
				a = &sumAgg{first: sm.T, last: sm.T, pids: map[uint32]bool{}}
				aggs[pr.Name] = a
			}
			a.last = sm.T
			if pr.Path != "" {
				a.path = pr.Path
			}
			a.pids[pr.PID] = true
			if pr.Pct > 0 {
				a.timeActive += sec
				a.sum += pr.Pct
				a.activeN++
				if pr.Pct > a.max {
					a.max = pr.Pct
				}
			}
			if pr.Pct >= float64(th) {
				a.timeAbove += sec
			}
		}
	}
	displayAdapter := s.adapterNames[adapter]
	s.mu.RUnlock()

	items := make([]sumItem, 0, len(aggs))
	for name, a := range aggs {
		pids := make([]uint32, 0, len(a.pids))
		for p := range a.pids {
			pids = append(pids, p)
		}
		sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
		avg := 0.0
		if a.activeN > 0 {
			avg = a.sum / float64(a.activeN)
		}
		items = append(items, sumItem{
			Name: name, Path: a.path,
			TimeAbove: a.timeAbove, TimeActive: a.timeActive,
			Max: a.max, Avg: avg,
			FirstSeen: a.first.UnixMilli(), LastSeen: a.last.UnixMilli(),
			PIDs: pids,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].TimeAbove != items[j].TimeAbove {
			return items[i].TimeAbove > items[j].TimeAbove
		}
		return items[i].TimeActive > items[j].TimeActive
	})
	writeJSON(w, map[string]interface{}{"adapter": displayAdapter, "threshold": th, "items": items})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", 405)
		return
	}
	s.stopOnce.Do(func() { s.stopCh <- struct{}{} })
	writeJSON(w, map[string]interface{}{"ok": true})
}

// ---------- 自检 ----------

func selfTest(interval time.Duration) {
	fmt.Println("== GPU 监控自检 ==")
	src, err := detectSource(interval)
	if err != nil {
		fmt.Println("数据源探测失败:", err)
		os.Exit(1)
	}
	fmt.Println("数据源:", src.Name())
	fmt.Printf("预热采集 (等待 %s ...)...\n", interval)
	src.Collect()
	time.Sleep(interval + 500*time.Millisecond)
	procs, totals, err := src.Collect()
	if err != nil {
		fmt.Println("采集失败:", err)
		os.Exit(1)
	}
	if len(totals) > 0 {
		for a, v := range totals {
			fmt.Printf("总占用 [%s]: %.1f%%\n", a, v)
		}
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].Pct > procs[j].Pct })
	fmt.Printf("进程占用 (共 %d 个, 显示前 10):\n", len(procs))
	for i, p := range procs {
		if i >= 10 {
			break
		}
		fmt.Printf("  PID %-8d %6.1f%%\n", p.PID, p.Pct)
	}
	fmt.Println("自检完成。")
}

// ---------- main ----------

func main() {
	port := flag.Int("port", 7777, "Web 端口")
	interval := flag.Duration("interval", time.Second, "采样间隔")
	duration := flag.Duration("duration", 6*time.Hour, "监控时长, 0 = 不限时")
	csvPath := flag.String("csv", "", "CSV 文件路径 (默认: exe 同目录 gpu-monitor.csv)")
	selftest := flag.Bool("selftest", false, "自检模式: 验证数据源后退出")
	flag.Parse()

	if *selftest {
		selfTest(*interval)
		return
	}

	if *csvPath == "" {
		exe, _ := os.Executable()
		*csvPath = filepath.Join(filepath.Dir(exe), "gpu-monitor.csv")
	}
	f, err := os.Create(*csvPath)
	if err != nil {
		log.Fatalf("无法创建 CSV 文件: %v", err)
	}
	cw := csv.NewWriter(f)
	cw.Write([]string{"timestamp", "adapter", "total_pct", "pid", "process", "exe_path", "proc_pct", "vram_mb"})
	cw.Flush()

	src, err := detectSource(*interval)
	if err != nil {
		log.Fatalf("数据源探测失败: %v", err)
	}
	log.Printf("数据源: %s", src.Name())

	s := &Server{
		running:      true,
		startedAt:    time.Now(),
		duration:     *duration,
		interval:     *interval,
		adapters:     map[string]bool{},
		adapterNames: map[string]string{},
		pathCache:    map[uint32]procInfo{},
		src:          src,
		sourceName:   src.Name(),
		csvF:         f,
		csvW:         cw,
		csvPath:      *csvPath,
		stopCh:       make(chan struct{}),
	}

	// 显卡显示名
	var vcs []Win32VideoController
	if wmi.Query(qVideoCtrl, &vcs) == nil {
		for _, v := range vcs {
			if v.Name != "" {
				s.videoNames = append(s.videoNames, v.Name)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			b, _ := webFS.ReadFile("web/index.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(b)
		case "/app.js":
			b, _ := webFS.ReadFile("web/app.js")
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/api/snapshot", s.handleSnapshot)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/summary", s.handleSummary)
	mux.HandleFunc("/api/stop", s.handleStop)
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, s.csvPath)
	})

	go s.collectLoop(*interval, *duration)

	log.Printf("监控开始: 间隔 %s, 时长 %s, CSV: %s", *interval, *duration, *csvPath)
	log.Printf("Web 界面: http://localhost:%d", *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), mux))
}
