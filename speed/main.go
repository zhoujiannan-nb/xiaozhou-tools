package main

import (
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

//go:embed ffmpeg.exe
var ffmpegFS embed.FS

const (
	ofilmTitle        = "选择视频文件"
	maxPath           = 1024
	OFN_FILEMUSTEXIST = 0x00001000
	OFN_PATHMUSTEXIST = 0x00000800
	OFN_NOCHANGEDIR   = 0x00000008
)

// 构建文件过滤器 (需要双null结尾)
func buildFilter() *uint16 {
	// "视频文件\0*.mp4;*.avi;*.mkv;*.mov\0所有文件\0*.*\0\0"
	parts := []string{
		"视频文件",
		"*.mp4;*.avi;*.mkv;*.mov;*.wmv;*.flv;*.webm",
		"所有文件",
		"*.*",
	}
	var buf []uint16
	for _, p := range parts {
		buf = append(buf, syscall.StringToUTF16(p)...)
		buf = append(buf, 0) // null separator
	}
	buf = append(buf, 0) // double null terminator
	return &buf[0]
}

var (
	user32              = syscall.NewLazyDLL("user32.dll")
	comdlg32            = syscall.NewLazyDLL("comdlg32.dll")
	procGetOpenFileName = comdlg32.NewProc("GetOpenFileNameW")
	procMessageBox      = user32.NewProc("MessageBoxW")
)

type openFILENAME struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	Flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	FlagsEx           uint32
}

func toUint16Ptr(s string) *uint16 {
	return syscall.StringToUTF16Ptr(s)
}

func selectFile() (string, error) {
	buf := make([]uint16, maxPath)
	var fn openFILENAME
	fn.lStructSize = uint32(unsafe.Sizeof(fn))
	fn.lpstrTitle = toUint16Ptr(ofilmTitle)
	fn.lpstrFilter = buildFilter()
	fn.lpstrFile = &buf[0]
	fn.nMaxFile = maxPath
	fn.Flags = OFN_FILEMUSTEXIST | OFN_PATHMUSTEXIST | OFN_NOCHANGEDIR

	ret, _, err := procGetOpenFileName.Call(uintptr(unsafe.Pointer(&fn)))
	if ret == 0 {
		return "", fmt.Errorf("用户取消选择: %v", err)
	}
	return syscall.UTF16ToString(buf), nil
}

func showMsg(title, msg string, flags uintptr) {
	procMessageBox.Call(0, uintptr(unsafe.Pointer(toUint16Ptr(msg))),
		uintptr(unsafe.Pointer(toUint16Ptr(title))), flags)
}

func getVideoDuration(ffmpegPath, filePath string) (float64, error) {
	cmd := exec.Command(ffmpegPath, "-i", filePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// ffmpeg -i 会返回错误(因为没有输出文件)，但输出中有信息
	}
	text := string(output)

	// 查找 Duration 行
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "Duration:") {
			line = strings.TrimSpace(line)
			// Duration: 00:05:30.45, start: 0.000000, bitrate: 1234 kb/s
			parts := strings.Split(line, "Duration: ")
			if len(parts) > 1 {
				timeStr := strings.Split(parts[1], ",")[0] // "00:05:30.45"
				return parseDuration(timeStr)
			}
		}
	}
	return 0, fmt.Errorf("无法获取视频时长")
}

func parseDuration(s string) (float64, error) {
	// 格式: HH:MM:SS.ms
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("无效的时间格式: %s", s)
	}

	hours, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, err
	}
	minutes, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, err
	}
	seconds, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return 0, err
	}

	return hours*3600 + minutes*60 + seconds, nil
}

func formatDuration(seconds float64) string {
	h := int(seconds) / 3600
	m := (int(seconds) % 3600) / 60
	s := seconds - float64(h*3600+m*60)
	return fmt.Sprintf("%02d:%02d:%05.2f", h, m, s)
}

func speedUpVideo(ffmpegPath, input, output string, speed float64) error {
	// ffmpeg 加速视频:
	// setpts=PTS/N 加速视频
	// atempo 只支持 0.5-100, 需要链式调用处理大倍速
	// 对于音频, atempo 支持 0.5-2.0, 超过需要链式调用

	// 构建视频滤镜
	videoFilter := fmt.Sprintf("setpts=PTS/%g", speed)

	// 构建音频滤镜 (atempo 链式)
	audioFilter := buildAtempoFilter(speed)

	args := []string{
		"-i", input,
		"-filter_complex",
		fmt.Sprintf("[0:v]%s[v];[0:a]%s[a]", videoFilter, audioFilter),
		"-map", "[v]",
		"-map", "[a]",
		"-y", // 覆盖输出文件
		output,
	}

	fmt.Printf("\n执行命令: ffmpeg %s\n\n", strings.Join(args, " "))
	fmt.Println("注意: ffmpeg输出的 'speed=xxx' 是编码处理速度，不是视频加速倍数")
	fmt.Println("视频加速效果请查看输出文件时长\n")

	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func buildAtempoFilter(speed float64) string {
	// atempo 只支持 0.5 到 100.0
	// 对于非常大的加速，我们可以链式调用或使用其他方法
	if speed <= 100.0 && speed >= 0.5 {
		return fmt.Sprintf("atempo=%g", speed)
	}

	// 对于超大倍速，链式调用
	var filters []string
	remaining := speed
	for remaining > 100.0 {
		filters = append(filters, "atempo=100.0")
		remaining /= 100.0
	}
	if remaining >= 0.5 {
		filters = append(filters, fmt.Sprintf("atempo=%g", remaining))
	} else {
		filters = append(filters, "atempo=0.5")
	}
	return strings.Join(filters, ",")
}

func main() {
	fmt.Println("========================================")
	fmt.Println("    视频加速工具 v1.0")
	fmt.Println("========================================")
	fmt.Println()

	// 1. 选择视频文件
	fmt.Println("[1/4] 请选择视频文件...")
	filePath, err := selectFile()
	if err != nil {
		showMsg("错误", err.Error(), 0)
		return
	}
	fmt.Printf("已选择: %s\n", filePath)

	// 2. 提取 ffmpeg
	fmt.Println("\n[2/4] 准备 ffmpeg...")
	tempDir := os.TempDir()
	ffmpegPath := filepath.Join(tempDir, "speed_ffmpeg.exe")

	ffmpegData, err := ffmpegFS.ReadFile("ffmpeg.exe")
	if err != nil {
		showMsg("错误", "无法提取 ffmpeg: "+err.Error(), 0)
		return
	}

	err = os.WriteFile(ffmpegPath, ffmpegData, 0755)
	if err != nil {
		showMsg("错误", "无法写入 ffmpeg: "+err.Error(), 0)
		return
	}
	defer os.Remove(ffmpegPath) // 清理临时文件
	fmt.Println("ffmpeg 已准备就绪")

	// 3. 获取视频时长
	fmt.Println("\n[3/4] 分析视频信息...")
	duration, err := getVideoDuration(ffmpegPath, filePath)
	if err != nil {
		showMsg("错误", "无法获取视频时长: "+err.Error(), 0)
		return
	}
	fmt.Printf("当前视频时长: %s (%.2f 秒)\n", formatDuration(duration), duration)

	// 4. 输入目标时长
	fmt.Println("\n[4/4] 设定目标时长")
	fmt.Println("请输入目标时长 (格式: HH:MM:SS 或 秒数)")
	fmt.Println("例如: 00:03:00 表示3分钟, 或直接输入 180 表示180秒")
	fmt.Print("目标时长: ")

	var input string
	fmt.Scanln(&input)

	var targetDuration float64
	if strings.Contains(input, ":") {
		targetDuration, err = parseDuration(input)
		if err != nil {
			showMsg("错误", "无效的时间格式: "+input, 0)
			return
		}
	} else {
		targetDuration, err = strconv.ParseFloat(input, 64)
		if err != nil {
			showMsg("错误", "无效的数字: "+input, 0)
			return
		}
	}

	if targetDuration <= 0 {
		showMsg("错误", "目标时长必须大于0", 0)
		return
	}

	speed := duration / targetDuration
	fmt.Printf("\n========================================\n")
	fmt.Printf("当前时长: %s\n", formatDuration(duration))
	fmt.Printf("目标时长: %s\n", formatDuration(targetDuration))
	fmt.Printf("加速倍数: %.2fx\n", speed)
	fmt.Printf("========================================\n")

	if speed <= 1.0 {
		showMsg("提示", "目标时长 >= 当前时长，无需加速", 0)
		return
	}

	// 5. 生成输出文件名
	dir := filepath.Dir(filePath)
	ext := filepath.Ext(filePath)
	base := filepath.Base(filePath)
	base = strings.TrimSuffix(base, ext)
	outputPath := filepath.Join(dir, fmt.Sprintf("%s_speed%.1fx%s", base, speed, ext))

	fmt.Printf("\n输出文件: %s\n", outputPath)
	fmt.Println("\n开始处理...")

	start := time.Now()
	err = speedUpVideo(ffmpegPath, filePath, outputPath, speed)
	elapsed := time.Since(start)

	if err != nil {
		showMsg("错误", "视频处理失败: "+err.Error(), 0)
		return
	}

	fmt.Printf("\n处理完成! 耗时: %s\n", elapsed.Round(time.Second))
	showMsg("完成", fmt.Sprintf("视频加速完成!\n\n输出文件:\n%s\n\n加速倍数: %.2fx\n处理耗时: %s", outputPath, speed, elapsed.Round(time.Second)), 0)
}
