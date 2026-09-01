# gpu-monitor

长时间（默认 **6 小时**）监控 Windows 上**每个进程的 GPU 占用**，找出"任务管理器里那 10% 是谁在吃、各吃了多久"。
Web 界面内嵌在 exe 里，启动后浏览器打开 `http://localhost:7777` 即可看图。

## 数据源（自动探测，与任务管理器同源）

| 优先级 | 数据源 | 适用系统 |
|---|---|---|
| 1 | WMI Raw 类 `Win32_PerfRawData_GPUPerformanceCounters_GPUEngine`（每进程+每引擎，累计值差值法，实例名解析 `pid_XXXX`） | Win11 24H2+ |
| 2 | WMI 格式化类 `Win32_PerfFormattedData_GPUPerformanceCounters_GPUProcessUtilization`（直接百分比） | Win10 |
| 3 | WMI Raw 类 `Win32_PerfRawData_GPUPerformanceCounters_GPUProcess`（差值法） | Win10 兜底 |

显存：`Win32_PerfRawData_GPUPerformanceCounters_GPUProcessMemory`（尽力而为，读不到不影响主功能）。

> 为什么不用 PDH？部分系统（VM/受保护环境）会拦截普通进程的 `PdhOpenQuery`，WMI 不受影响。
> 为什么差值法？Raw 类的 `UtilizationPercentage` 是累计 100ns tick，两次采样相除即得百分比。

## 编译

```
go build -o gpu-monitor.exe .
```

## 用法

```
gpu-monitor.exe                  # 监控 6 小时, Web 开在 :7777
gpu-monitor.exe -selftest        # 自检: 验证数据源可用后退出 (换机器先跑这个)
gpu-monitor.exe -duration 1h     # 监控 1 小时
gpu-monitor.exe -interval 2s     # 每 2 秒采样一次
gpu-monitor.exe -port 8080       # 换端口
gpu-monitor.exe -csv D:\data\gpu.csv   # 换 CSV 路径 (默认 exe 同目录 gpu-monitor.csv)
```

## Web 界面（http://localhost:7777）

- **当前状态**：GPU 总占用 + 每进程 GPU% / 显存 / 完整 exe 路径
- **占用趋势**：canvas 折线图（总占用 + 前 5 大进程），5m/30m/2h/6h/全部，红色虚线 = 阈值，悬停看数值
- **进程统计（核心）**：按"GPU 占用 ≥ 阈值（默认 10%）的**累计时长**"排序，
  附活跃时长、峰值、均值、首次/最后出现时间、exe 路径
- 多显卡时可在右上角切换（自动选累计占用最高的为主卡）
- 页面上可直接**停止监控**、**下载 CSV**

## CSV 格式

每进程每秒一行：

```
timestamp,adapter,total_pct,pid,process,exe_path,proc_pct,vram_mb
2026-08-31 17:38:43,0x00000000_0x00012710,31.8,4780,dwm.exe,,31.8,1044.4
```

6 小时 ≈ 2 万行以内（只记录有 GPU 活动的进程），Excel 可直接打开。

## 说明

- 首次采样无基线，第 2 秒起出数据。
- 进程名/路径每 60 秒刷新一次缓存；进程退出后其历史统计仍保留（按进程名聚合，PID 变化不影响）。
- 该计数器由 WDDM 驱动上报，个别驱动版本对某些进程的归因可能不准，
  此时可用 NVIDIA 的 NVML（`nvmlDeviceGetProcessUtilization`）交叉验证。
- 测试脚本：`test-web.ps1`（启动 1 分钟监控并自动请求所有 API）。
