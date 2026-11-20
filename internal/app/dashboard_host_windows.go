//go:build windows

package app

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Windows 开发机上没有 /proc，仪表盘原来整卡片显示 0.00 B / 0.0%。
// 这里用三个 Win32 调用取同一份数据：
//
//	GlobalMemoryStatusEx → 物理内存总量/可用量
//	GetSystemTimes       → CPU 空闲/内核/用户累计时间
//	GetIfTable2          → 各接口收发字节数（used with FreeMibTable）
//
// 都只读本机，不发请求、不依赖额外依赖。
var (
	windowsKernel32 = syscall.NewLazyDLL("kernel32.dll")
	windowsIPHelper = syscall.NewLazyDLL("iphlpapi.dll")
	windowsPSAPI    = syscall.NewLazyDLL("psapi.dll")

	procGlobalMemoryStatusEx = windowsKernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes       = windowsKernel32.NewProc("GetSystemTimes")
	procGetCurrentProcess    = windowsKernel32.NewProc("GetCurrentProcess")
	procGetIfTable2          = windowsIPHelper.NewProc("GetIfTable2")
	procFreeMibTable         = windowsIPHelper.NewProc("FreeMibTable")
	procGetProcessMemoryInfo = windowsPSAPI.NewProc("GetProcessMemoryInfo")
)

// memoryStatusEx 对应 Win32 MEMORYSTATUSEX。
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// hostMemoryInfo 返回物理内存总量与可用量，单位字节。
//
// cached 固定 0，这是**跟随 V2**而不是偷懒：V2 用 psutil.virtual_memory()，
// 而 psutil 在 Windows 上 `cached`/`buffers` 恒为 0（该口径只在 Linux/BSD 有）。
// 也就是说 V2 自己跑在 Windows 上，这个数字同样是 0。不用 GetPerformanceInfo
// 的 SystemCache 或 standby 列表去凑一个近似值——那些含内核自身占用，
// 与 Linux 的 Cached+Buffers 不可比，凑出来会和 V2 对不上。
func hostMemoryInfo() (total, available, cached int64, err error) {
	var status memoryStatusEx
	status.Length = uint32(unsafe.Sizeof(status))
	ok, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ok == 0 {
		return 0, 0, 0, fmt.Errorf("GlobalMemoryStatusEx 失败: %w", callErr)
	}
	return int64(status.TotalPhys), int64(status.AvailPhys), 0, nil
}

// processMemoryCounters 对应 Win32 PROCESS_MEMORY_COUNTERS（x64 下 SIZE_T 为 8 字节）。
type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// processRSSBytes 读取本进程常驻内存（WorkingSetSize）。
//
// V2 仪表盘的 `used` 是 psutil.Process().memory_info().rss，psutil 在 Windows 上
// 正是把 rss 映射到 WorkingSetSize，所以这里取同一个计数器，数字和原版一致。
func processRSSBytes() (int64, error) {
	handle, _, handleErr := procGetCurrentProcess.Call()
	if handle == 0 {
		return 0, fmt.Errorf("GetCurrentProcess 失败: %w", handleErr)
	}
	var counters processMemoryCounters
	counters.Cb = uint32(unsafe.Sizeof(counters))
	ok, _, callErr := procGetProcessMemoryInfo.Call(
		handle, uintptr(unsafe.Pointer(&counters)), uintptr(counters.Cb),
	)
	if ok == 0 {
		return 0, fmt.Errorf("GetProcessMemoryInfo 失败: %w", callErr)
	}
	return int64(counters.WorkingSetSize), nil
}

// filetime 对应 Win32 FILETIME：100 纳秒为单位的两个 32 位计数。
type filetime struct {
	low  uint32
	high uint32
}

func (f filetime) value() uint64 {
	return uint64(f.high)<<32 | uint64(f.low)
}

// hostCPUSample 用 GetSystemTimes 取累计时间。
//
// 注意 Windows 的「内核时间」已经包含空闲时间，所以 total = kernel + user，
// 与 /proc/stat 里「所有字段相加」等价，cpuPercent 的算法不用改。
func hostCPUSample() (cpuSample, error) {
	var idle, kernel, user filetime
	ok, _, callErr := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ok == 0 {
		return cpuSample{}, fmt.Errorf("GetSystemTimes 失败: %w", callErr)
	}
	return cpuSample{total: kernel.value() + user.value(), idle: idle.value()}, nil
}

// mibIfRow2 对应 Win32 MIB_IF_ROW2 的前缀部分，字段顺序与官方结构体一致，
// 只保留到 OutOctets 之后不再关心（GetIfTable2 按固定步长写入整行）。
type mibIfRow2 struct {
	InterfaceLuid               uint64
	InterfaceIndex              uint32
	InterfaceGuid               [16]byte
	Alias                       [257]uint16
	Description                 [257]uint16
	PhysicalAddressLength       uint32
	PhysicalAddress             [32]byte
	PermanentPhysicalAddress    [32]byte
	Mtu                         uint32
	Type                        uint32
	TunnelType                  uint32
	MediaType                   uint32
	PhysicalMediumType          uint32
	AccessType                  uint32
	DirectionType               uint32
	InterfaceAndOperStatusFlags byte
	_                           [3]byte
	OperStatus                  uint32
	AdminStatus                 uint32
	MediaConnectState           uint32
	NetworkGuid                 [16]byte
	ConnectionType              uint32
	TransmitLinkSpeed           uint64
	ReceiveLinkSpeed            uint64
	InOctets                    uint64
	InUcastPkts                 uint64
	InNUcastPkts                uint64
	InDiscards                  uint64
	InErrors                    uint64
	InUnknownProtos             uint64
	InUcastOctets               uint64
	InMulticastOctets           uint64
	InBroadcastOctets           uint64
	OutOctets                   uint64
}

// mibIfTable2 是 MIB_IF_TABLE2 的表头，后面紧跟 NumEntries 个 MIB_IF_ROW2。
type mibIfTable2 struct {
	NumEntries uint32
	_          uint32
}

const (
	// mibIfRow2Size 是 sizeof(MIB_IF_ROW2)：64 位 Windows 上固定 1352 字节。
	// 只用 Sizeof 取不到（结构体只声明了前缀），所以直接写死并按行步进。
	mibIfRow2Size = 1352
	// mibIfFlagFilterInterface 是 InterfaceAndOperStatusFlags 里的「过滤层」位。
	// 这些过滤层会镜像同一块网卡的计数，累加会把流量算成好几倍。
	mibIfFlagFilterInterface = 0x02
	// ifTypeSoftwareLoopback 对应 IF_TYPE_SOFTWARE_LOOPBACK，回环不计入。
	ifTypeSoftwareLoopback = 24
)

// hostNetSample 汇总所有物理/虚拟接口的收发字节总数，跳过过滤层与回环。
// 口径与 /proc/net/dev 一致：累计值，速率由 dashboardRuntime.Network 折算。
func hostNetSample() (netSample, error) {
	var table *mibIfTable2
	ret, _, _ := procGetIfTable2.Call(uintptr(unsafe.Pointer(&table)))
	if ret != 0 {
		return netSample{}, fmt.Errorf("GetIfTable2 失败: 错误码 %d", ret)
	}
	defer procFreeMibTable.Call(uintptr(unsafe.Pointer(table)))

	base := unsafe.Add(unsafe.Pointer(table), unsafe.Sizeof(*table))
	var sample netSample
	for index := 0; index < int(table.NumEntries); index++ {
		row := (*mibIfRow2)(unsafe.Add(base, index*mibIfRow2Size))
		if row.InterfaceAndOperStatusFlags&mibIfFlagFilterInterface != 0 {
			continue
		}
		if row.Type == ifTypeSoftwareLoopback {
			continue
		}
		sample.rx += int64(row.InOctets)
		sample.tx += int64(row.OutOctets)
	}
	return sample, nil
}
