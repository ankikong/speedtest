package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

type SelectedServer struct {
	Server Server  `json:"server"`
	RTTMs  float64 `json:"rtt_ms"`
	Auto   bool    `json:"auto"`
}

type TestReport struct {
	IPLoc    IPLoc          `json:"ip_loc"`
	MatchURL string         `json:"match_url,omitempty"`
	Selected SelectedServer `json:"selected"`
	Ping     PingStats      `json:"ping"`
	UUID     string         `json:"uuid,omitempty"`
	Download TransferResult `json:"download"`
	Upload   TransferResult `json:"upload"`
}

func main() {
	ipFlag := flag.String("ip", "", "外网 IP，留空自动获取")
	provinceFlag := flag.String("province", "", "省份，留空自动获取")
	cityFlag := flag.String("city", "", "城市，留空自动获取")
	wifiOperFlag := flag.String("wifioper", "", "WiFi 运营商描述，留空自动获取")
	mobileOperFlag := flag.String("mobileoperid", "0", "移动网络运营商代码")
	networkFlag := flag.Int("network", 4, "网络类型：4=WiFi，5=移动网络")
	ipv6Flag := flag.Bool("ipv6", false, "请求 IPv6 测速服务器")
	pkgFlag := flag.String("pkg", "com.cnspeedtest.globalspeed", "包名")
	appFlag := flag.String("app", "globalspeed", "短应用名")
	imeiFlag := flag.String("imei", "", "设备标识，留空自动生成")

	listFlag := flag.Bool("list", false, "只获取并打印匹配的服务器列表")
	serverFlag := flag.String("server", "", "手动指定服务器 hostid/ip/名称")
	jsonFlag := flag.Bool("json", false, "以 JSON 输出结果")
	decryptFlag := flag.String("decrypt", "", "解密本地 serverlist.json 并打印")
	selectOnly := flag.Bool("select-only", false, "只选择服务器并退出")
	enqueueOnly := flag.Bool("enqueue-only", false, "只排队并打印会话 UUID")
	noDequeue := flag.Bool("no-dequeue", false, "结束时不再释放会话")
	verboseFlag := flag.Bool("v", false, "打印请求 URL")

	bandwidthFlag := flag.Int("bandwidth", 0, "带宽参数，留空按网络类型选择")
	downFlag := flag.Int("down", 0, "下载线程数")
	upFlag := flag.Int("up", 0, "上传线程数")
	pointsFlag := flag.Int("points", 16, "采样点数")
	preheatFlag := flag.Int("preheat", 4, "预热采样点数")
	intervalFlag := flag.Duration("interval", 500*time.Millisecond, "采样间隔")
	maxWaitFlag := flag.Duration("max-wait", 20*time.Second, "单向测试最长等待")
	timeoutFlag := flag.Duration("timeout", 5*time.Second, "连接超时")
	pingCountFlag := flag.Int("ping-count", 10, "Ping 次数")
	flag.Parse()

	if *decryptFlag != "" {
		data, err := os.ReadFile(*decryptFlag)
		if err != nil {
			fatal(err)
		}
		out, err := decryptServerList(data)
		if err != nil {
			fatal(err)
		}
		os.Stdout.Write(out)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			fmt.Println()
		}
		return
	}

	bandwidth := *bandwidthFlag
	if bandwidth == 0 {
		if *networkFlag == 5 {
			bandwidth = 300
		} else {
			bandwidth = 200
		}
	}
	downThreads := *downFlag
	if downThreads == 0 {
		if *networkFlag == 5 {
			downThreads = 16
		} else {
			downThreads = 8
		}
	}
	upThreads := *upFlag
	if upThreads == 0 {
		if *networkFlag == 5 {
			upThreads = 6
		} else {
			upThreads = 4
		}
	}
	imei := *imeiFlag
	if imei == "" {
		imei = newIMEI()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := newHTTPClient(12 * time.Second)

	var loc IPLoc
	if *ipFlag == "" || *provinceFlag == "" || *cityFlag == "" || *wifiOperFlag == "" {
		got, err := fetchIPLoc(ctx, client)
		if err != nil {
			if *ipFlag == "" {
				fatal(fmt.Errorf("获取外网 IP/归属地失败: %w", err))
			}
			progress(*verboseFlag, "获取外网 IP/归属地失败: %v", err)
		} else {
			loc = got
		}
	}
	ip := firstNonEmpty(*ipFlag, loc.IP)
	province := provinceFullName(firstNonEmpty(*provinceFlag, loc.Province))
	city := cityFullName(firstNonEmpty(*cityFlag, loc.City))
	wifiOper := firstNonEmpty(*wifiOperFlag, loc.Oper)
	if ip == "" {
		fatal(fmt.Errorf("无法确定外网 IP，请使用 -ip 指定"))
	}
	if net.ParseIP(ip) == nil {
		fatal(fmt.Errorf("无效的外网 IP: %s", ip))
	}

	progress(*verboseFlag, "外网 IP: %s，归属地: %s %s，运营商: %s", ip, province, city, wifiOper)

	servers, matchURL, err := matchServers(ctx, client, ip, province, city, wifiOper, *mobileOperFlag, *networkFlag, *ipv6Flag, *pkgFlag)
	if err != nil {
		fatal(fmt.Errorf("获取服务器列表失败: %w", err))
	}
	if *verboseFlag {
		progress(true, "匹配接口: %s", matchURL)
	}

	if *listFlag {
		if *jsonFlag {
			out, _ := json.MarshalIndent(servers, "", "  ")
			fmt.Println(string(out))
			return
		}
		printServers(servers)
		return
	}

	choice, err := selectServer(servers, *serverFlag, *timeoutFlag)
	if err != nil {
		fatal(err)
	}
	if choice.Auto {
		progress(*verboseFlag, "自动选择 %s (%s:%s)，TCP 握手 %.2f ms", choice.Server.HostName, choice.Server.HostIP, choice.Server.Port, float64(choice.RTT.Microseconds())/1000.0)
	} else {
		progress(*verboseFlag, "手动选择 %s (%s:%s)", choice.Server.HostName, choice.Server.HostIP, choice.Server.Port)
	}

	report := TestReport{
		IPLoc:    loc,
		MatchURL: matchURL,
		Selected: SelectedServer{Server: choice.Server, RTTMs: float64(choice.RTT.Microseconds()) / 1000.0, Auto: choice.Auto},
	}

	if *selectOnly {
		if *jsonFlag {
			out, _ := json.MarshalIndent(report.Selected, "", "  ")
			fmt.Println(string(out))
			return
		}
		fmt.Printf("%s\t%s\t%s:%s\t%s\t%s\n", choice.Server.HostID, choice.Server.HostName, choice.Server.HostIP, choice.Server.Port, choice.Server.PName, choice.Server.City)
		return
	}

	progress(*verboseFlag, "ICMP/TCP Ping %s", choice.Server.HostIP)
	report.Ping = runPing(choice.Server.HostIP, choice.Server.Port, *pingCountFlag)

	progress(*verboseFlag, "排队测速会话 (dovalid)")
	uuid, enqueueURL, err := enqueue(ctx, client, choice.Server, imei, *appFlag, *pkgFlag, bandwidth)
	if *verboseFlag {
		progress(true, "排队接口: %s", enqueueURL)
	}
	if err != nil {
		outputReport(*jsonFlag, report)
		fatal(fmt.Errorf("排队失败: %w", err))
	}
	report.UUID = uuid
	progress(*verboseFlag, "会话 UUID: %s", uuid)

	if *enqueueOnly {
		fmt.Println(uuid)
		return
	}

	cfgDown := TransferConfig{
		Server:      choice.Server,
		UUID:        uuid,
		Threads:     downThreads,
		Preheat:     *preheatFlag,
		Points:      *pointsFlag,
		Interval:    *intervalFlag,
		DialTimeout: *timeoutFlag,
		MaxWait:     *maxWaitFlag,
	}
	cfgUp := cfgDown
	cfgUp.Threads = upThreads

	progress(*verboseFlag, "开始下行测试，线程数 %d", downThreads)
	report.Download, err = runDownload(ctx, cfgDown)
	if err != nil {
		progress(true, "下行测试失败: %v", err)
	} else {
		progress(*verboseFlag, "下行完成: %.2f Mbps", report.Download.AvgMbps)
	}

	progress(*verboseFlag, "开始上行测试，线程数 %d", upThreads)
	report.Upload, err = runUpload(ctx, cfgUp)
	if err != nil {
		progress(true, "上行测试失败: %v", err)
	} else {
		progress(*verboseFlag, "上行完成: %.2f Mbps", report.Upload.AvgMbps)
	}

	if !*noDequeue {
		if err := dequeue(ctx, client, choice.Server, uuid); err != nil {
			progress(*verboseFlag, "释放会话失败: %v", err)
		}
	}

	outputReport(*jsonFlag, report)
}

func outputReport(asJSON bool, report TestReport) {
	if asJSON {
		out, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(out))
		return
	}
	fmt.Printf("服务器: %s (%s) %s:%s %s %s\n", report.Selected.Server.HostName, report.Selected.Server.HostID, report.Selected.Server.HostIP, report.Selected.Server.Port, report.Selected.Server.PName, report.Selected.Server.City)
	if report.Ping.OK {
		fmt.Printf("Ping: avg %.2f ms, jitter %.2f ms, loss %.2f%% (%s)\n", report.Ping.Avg, report.Ping.Jitter, report.Ping.Loss, report.Ping.Method)
	}
	if report.Download.AvgMbps > 0 {
		fmt.Printf("下行: %.2f Mbps (max %.2f Mbps, %d 线程)\n", report.Download.AvgMbps, report.Download.MaxMbps, report.Download.Threads)
	}
	if report.Upload.AvgMbps > 0 {
		fmt.Printf("上行: %.2f Mbps (max %.2f Mbps, %d 线程)\n", report.Upload.AvgMbps, report.Upload.MaxMbps, report.Upload.Threads)
	}
}

func printServers(servers []Server) {
	for i, s := range servers {
		fmt.Printf("%d\t%s\t%s\t%s:%s\t%s\t%s\n", i+1, s.HostID, s.HostName, s.HostIP, s.Port, s.PName, s.City)
	}
}

func newIMEI() string {
	b := make([]byte, 16)
	rand.Read(b)
	s := strings.ToUpper(hex.EncodeToString(b))
	return "TS" + s[:16]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func progress(show bool, format string, args ...interface{}) {
	if !show {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}
