package main

import (
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type PingStats struct {
	Min    float64 `json:"min_ms"`
	Avg    float64 `json:"avg_ms"`
	Max    float64 `json:"max_ms"`
	Mdev   float64 `json:"mdev_ms"`
	Jitter float64 `json:"jitter_ms"`
	Loss   float64 `json:"loss_percent"`
	Sent   int     `json:"sent"`
	Recv   int     `json:"recv"`
	Method string  `json:"method"`
	OK     bool    `json:"ok"`
}

var (
	rePingTime = regexp.MustCompile(`time[=<]([\d.]+)\s*ms`)
	rePingStat = regexp.MustCompile(`(\d+)\s+packets transmitted,\s+(\d+)\s+received,\s+([\d.]+)%\s+packet loss`)
	rePingRTT  = regexp.MustCompile(`(?:rtt|round-trip)\s+min/avg/max/(?:mdev|stddev)\s*=\s*([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+)`)
)

func runPing(ip, port string, count int) PingStats {
	if stats, ok := icmpPing(ip, count); ok {
		return stats
	}
	return tcpPingStats(ip, port, count)
}

func icmpPing(ip string, count int) (PingStats, bool) {
	if _, err := exec.LookPath("ping"); err != nil {
		return PingStats{}, false
	}
	args := []string{"-i", "0.2", "-s", "64", "-c", strconv.Itoa(count), "-W", "3", ip}
	if strings.Contains(ip, ":") {
		args = append([]string{"-6"}, args...)
	}
	cmd := exec.Command("ping", args...)
	cmd.Env = []string{"LC_ALL=C", "LANG=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.CombinedOutput()
	text := string(out)
	if err != nil && text == "" {
		return PingStats{}, false
	}
	stats := PingStats{Method: "icmp"}
	var rtts []float64
	for _, m := range rePingTime.FindAllStringSubmatch(text, -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err == nil {
			rtts = append(rtts, v)
		}
	}
	if m := rePingStat.FindStringSubmatch(text); m != nil {
		stats.Sent, _ = strconv.Atoi(m[1])
		stats.Recv, _ = strconv.Atoi(m[2])
		stats.Loss, _ = strconv.ParseFloat(m[3], 64)
	}
	if m := rePingRTT.FindStringSubmatch(text); m != nil {
		stats.Min, _ = strconv.ParseFloat(m[1], 64)
		stats.Avg, _ = strconv.ParseFloat(m[2], 64)
		stats.Max, _ = strconv.ParseFloat(m[3], 64)
		stats.Mdev, _ = strconv.ParseFloat(m[4], 64)
	}
	if len(rtts) > 0 {
		if stats.Min == 0 && stats.Avg == 0 && stats.Max == 0 {
			stats.Min, stats.Max = rtts[0], rtts[0]
			sum := 0.0
			for _, v := range rtts {
				if v < stats.Min {
					stats.Min = v
				}
				if v > stats.Max {
					stats.Max = v
				}
				sum += v
			}
			stats.Avg = sum / float64(len(rtts))
		}
		if len(rtts) > 1 {
			jitter := 0.0
			for i := 1; i < len(rtts); i++ {
				jitter += math.Abs(rtts[i] - rtts[i-1])
			}
			stats.Jitter = jitter / float64(len(rtts)-1)
		}
	}
	stats.OK = stats.Recv > 0 && stats.Avg > 0
	return stats, stats.OK
}

func tcpPingStats(ip, port string, count int) PingStats {
	stats := PingStats{Method: "tcp", Sent: count}
	var rtts []float64
	for i := 0; i < count; i++ {
		start := time.Now()
		rtt, ok := tcpPing(ip, port, 1, 3*time.Second)
		if ok > 0 {
			ms := float64(rtt.Microseconds()) / 1000.0
			rtts = append(rtts, ms)
			_ = start
		}
	}
	stats.Recv = len(rtts)
	if count > 0 {
		stats.Loss = float64(count-len(rtts)) * 100 / float64(count)
	}
	if len(rtts) == 0 {
		return stats
	}
	stats.Min, stats.Max = rtts[0], rtts[0]
	sum := 0.0
	for _, v := range rtts {
		if v < stats.Min {
			stats.Min = v
		}
		if v > stats.Max {
			stats.Max = v
		}
		sum += v
	}
	stats.Avg = sum / float64(len(rtts))
	if len(rtts) > 1 {
		j := 0.0
		for i := 1; i < len(rtts); i++ {
			j += math.Abs(rtts[i] - rtts[i-1])
		}
		stats.Jitter = j / float64(len(rtts)-1)
	}
	stats.OK = true
	return stats
}
