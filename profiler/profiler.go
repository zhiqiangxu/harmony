package profiler

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/shirou/gopsutil/process"
	"github.com/simple-rules/harmony-benchmark/log"
)

type Profiler struct {
	// parameters
	logger           log.Logger
	pid              int32
	shardID          string
	MetricsReportURL string
	// Internal
	proc *process.Process
}

var singleton *Profiler
var once sync.Once

func GetProfiler() *Profiler {
	once.Do(func() {
		singleton = &Profiler{}
	})
	return singleton
}

func (profiler *Profiler) Config(logger log.Logger, shardID string, metricsReportURL string) {
	profiler.logger = logger
	profiler.pid = int32(os.Getpid())
	profiler.shardID = shardID
	profiler.MetricsReportURL = metricsReportURL
}

func (profiler *Profiler) LogMemory() {
	for {
		// log mem usage
		info, _ := profiler.proc.MemoryInfo()
		memMap, _ := profiler.proc.MemoryMaps(false)
		profiler.logger.Info("Mem Report", "info", info, "map", memMap, "shardID", profiler.shardID)

		time.Sleep(3 * time.Second)
	}
}

func (profiler *Profiler) LogCPU() {
	for {
		// log cpu usage
		percent, _ := profiler.proc.CPUPercent()
		times, _ := profiler.proc.Times()
		profiler.logger.Info("CPU Report", "percent", percent, "times", times, "shardID", profiler.shardID)

		time.Sleep(3 * time.Second)
	}
}

func (profiler *Profiler) LogMetrics(metrics url.Values) {
	body := bytes.NewBufferString(metrics.Encode())
	rsp, err := http.Post(profiler.MetricsReportURL, "application/x-www-form-urlencoded", body)
	if err == nil {
		defer rsp.Body.Close()
	}
}

func (profiler *Profiler) Start() {
	profiler.proc, _ = process.NewProcess(profiler.pid)
	go profiler.LogCPU()
	go profiler.LogMemory()
}
