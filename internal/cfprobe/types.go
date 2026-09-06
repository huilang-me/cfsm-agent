package cfprobe

import "time"

const (
	serviceNameDefault          = "cf-probe"
	legacyAgentVersion          = "1.0.0"
	maxTrafficCorrectionGB      = 1000000
	autoUpdateDelay             = 60 * time.Second
	configSchemaVersion         = "7"
	defaultReportIntervalSec    = 60
	defaultWSSReportIntervalSec = 2
	minWSSReportIntervalSec     = 1
	maxWSSReportIntervalSec     = 5
	connectionModeAuto          = "auto"
	connectionModeHTTP          = "http"
	pingModeTCP                 = "tcp"
	pingModeICMP                = "icmp"
)

type Config struct {
	ServerID        string
	Secret          string
	WorkerURL       string
	CollectInterval int
	ReportInterval  int
	CTNode          string
	CUNode          string
	CMNode          string
	BDNode          string
	Node1           string
	Node2           string
	Node3           string
	Node4           string
	Interface       string
	ResetDay        int
	ConnectionMode  string
	PingMode        string
	AutoUpdate      bool
	UpdateProxy     string
	// ConfigMD5 mirrors the worker dynamic config version; local fields like
	// AutoUpdate and UpdateProxy are intentionally outside that comparison.
	ConfigMD5 string
}

type Paths struct {
	ServiceName     string
	BinaryFile      string
	ConfigDir       string
	ConfigFile      string
	TrafficFile     string
	OldTrafficFile  string
	PIDFile         string
	LogFile         string
	ServiceFile     string
	DebugEnvFile    string
	LaunchdLabel    string
	LaunchdUserFile string
	LaunchdRootFile string
	UserMode        bool
	RunUser         string
	RunUID          int
	HomeDir         string
}

type InstallOptions struct {
	Config
	Debug          bool
	RXCorrectionGB string
	TXCorrectionGB string
	NoStart        bool
	Explicit       map[string]bool
}

type ProbeResult struct {
	RTTMs int
	Loss  int
	OK    bool
}

type ProbeSnapshot struct {
	IPv4  string
	IPv6  string
	CT    ProbeResult
	CU    ProbeResult
	CM    ProbeResult
	BD    ProbeResult
	Node1 ProbeResult
	Node2 ProbeResult
	Node3 ProbeResult
	Node4 ProbeResult
}

type Metrics struct {
	CPU          string
	RAMTotal     string
	RAMUsed      string
	SwapTotal    string
	SwapUsed     string
	DiskTotal    string
	DiskUsed     string
	Disk         DiskIOStats
	LoadAvg      string
	BootTime     string
	NetRX        string
	NetTX        string
	NetRXMonthly string
	NetTXMonthly string
	NetInSpeed   string
	NetOutSpeed  string
	OS           string
	Arch         string
	Kernel       string
	CPUInfo      string
	CPUCores     string
	GPUInfo      any
	Processes    string
	TCPConn      string
	UDPConn      string
	IPv4         string
	IPv6         string
	PingCT       any
	PingCU       any
	PingCM       any
	PingBD       any
	PingNode1    any
	PingNode2    any
	PingNode3    any
	PingNode4    any
	LossCT       any
	LossCU       any
	LossCM       any
	LossBD       any
	LossNode1    any
	LossNode2    any
	LossNode3    any
	LossNode4    any
}

type BasicStats struct {
	MemTotalMB  uint64
	MemUsedMB   uint64
	SwapTotalMB uint64
	SwapUsedMB  uint64
	DiskTotalMB uint64
	DiskUsedMB  uint64
	DiskDevices []DiskDeviceRef
	LoadAvg     string
	BootTimeMS  int64
	OSName      string
	Arch        string
	Kernel      string
	CPUInfo     string
	CPUCores    int
	GPUInfo     any
	Processes   int
	TCPConn     int
	UDPConn     int
}

type MemoryStats struct {
	MemTotalMB  uint64
	MemUsedMB   uint64
	SwapTotalMB uint64
	SwapUsedMB  uint64
}

type NetBytes struct {
	RX uint64
	TX uint64
}

type DiskDeviceRef struct {
	Key   string
	Major uint64
	Minor uint64
}

type DiskIOCounters struct {
	ReadBytes   uint64
	WriteBytes  uint64
	ReadOps     uint64
	WriteOps    uint64
	ReadTimeMS  uint64
	WriteTimeMS uint64
	IOTicksMS   uint64
	DeviceCount int
	Fingerprint string
}

type DiskIOStats struct {
	ReadBps   uint64  `json:"read_bps"`
	WriteBps  uint64  `json:"write_bps"`
	ReadIOPS  float64 `json:"read_iops"`
	WriteIOPS float64 `json:"write_iops"`
	AwaitMS   float64 `json:"await_ms"`
	Util      float64 `json:"util"`
}
