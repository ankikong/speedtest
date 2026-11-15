package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type TransferResult struct {
	AvgMbps  float64   `json:"avg_mbps"`
	MaxMbps  float64   `json:"max_mbps"`
	Bytes    int64     `json:"bytes"`
	Seconds  float64   `json:"seconds"`
	Threads  int       `json:"threads"`
	Samples  []float64 `json:"samples"`
	Protocol string    `json:"protocol"`
}

type TransferConfig struct {
	Server      Server
	UUID        string
	Threads     int
	Preheat     int
	Points      int
	Interval    time.Duration
	DialTimeout time.Duration
	MaxWait     time.Duration
}

type errBox struct {
	mu  sync.Mutex
	err error
}

func (b *errBox) set(err error) {
	b.mu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.mu.Unlock()
}

func (b *errBox) get() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

const downloadUA = "Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/100.0.4896.60 Safari/537.36"
const uploadUA = "Dalvik/1.6.0 (Linux; U; Android 4.2.2; GT-I9505 Build/JDQ39)"

func hostForURL(s Server) string {
	if s.IPv6 {
		return "[" + s.HostIP + "]"
	}
	return s.HostIP
}

func runDownload(ctx context.Context, cfg TransferConfig) (TransferResult, error) {
	return runTransfer(ctx, cfg, true)
}

func runUpload(ctx context.Context, cfg TransferConfig) (TransferResult, error) {
	return runTransfer(ctx, cfg, false)
}

func runTransfer(ctx context.Context, cfg TransferConfig, down bool) (TransferResult, error) {
	if cfg.Threads <= 0 {
		cfg.Threads = 1
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 500 * time.Millisecond
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 20 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}

	addr := net.JoinHostPort(cfg.Server.HostIP, cfg.Server.Port)
	var conns []net.Conn
	dialer := net.Dialer{Timeout: cfg.DialTimeout}
	for i := 0; i < cfg.Threads; i++ {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			continue
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.SetNoDelay(true)
		}
		conns = append(conns, conn)
	}
	if len(conns) == 0 {
		return TransferResult{}, errors.New("server connect failed")
	}
	if len(conns)*2 < cfg.Threads {
		for _, c := range conns {
			c.Close()
		}
		return TransferResult{}, fmt.Errorf("too few connections: %d/%d", len(conns), cfg.Threads)
	}

	var total atomic.Int64
	var firstErr errBox
	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	t0 := time.Now()
	finished := make(chan []float64, 1)
	go func() {
		ticker := time.NewTicker(cfg.Interval)
		defer ticker.Stop()
		index := 0
		var prevTotal int64
		var lastDelta int64
		var samples []float64
		for {
			select {
			case <-ctx.Done():
				finished <- samples
				return
			case <-ticker.C:
			}
			index++
			cur := total.Load()
			delta := cur - prevTotal
			prevTotal = cur
			var speed float64
			if lastDelta > 0 {
				speed = bytesToMbps(lastDelta+delta, cfg.Interval*2)
			} else {
				speed = bytesToMbps(delta, cfg.Interval)
			}
			lastDelta = delta
			if index > cfg.Preheat {
				samples = append(samples, speed)
			}
			if index >= cfg.Preheat+cfg.Points {
				finished <- samples
				return
			}
		}
	}()

	requestStart := time.Now().Unix()
	for _, conn := range conns {
		c := conn
		go func() {
			defer c.Close()
			if down {
				runDownloadConn(c, cfg, requestStart, &total, &firstErr, stop)
			} else {
				runUploadConn(c, cfg, &total, &firstErr, stop)
			}
		}()
	}

	var samples []float64
	select {
	case samples = <-finished:
	case <-time.After(cfg.MaxWait):
		cancel()
		samples = <-finished
	}
	close(stop)
	for _, c := range conns {
		c.Close()
	}

	res := TransferResult{
		Threads:  len(conns),
		Bytes:    total.Load(),
		Seconds:  time.Since(t0).Seconds(),
		Samples:  samples,
		Protocol: "http",
	}
	if len(samples) > 0 {
		res.AvgMbps = averageTop(samples, 3)
		res.MaxMbps = maxFloat(samples)
	}
	if res.AvgMbps <= 0.001 {
		if err := firstErr.get(); err != nil {
			return res, err
		}
		if down {
			return res, errors.New("download speed is zero")
		}
		return res, errors.New("upload speed is zero")
	}
	return res, nil
}

func runDownloadConn(conn net.Conn, cfg TransferConfig, reqStart int64, total *atomic.Int64, firstErr *errBox, stop <-chan struct{}) {
	req := fmt.Sprintf("GET /speed/File(1G).dl?r=%d&key=%s HTTP/1.1\r\nAccept: */*\r\nConnection: close\r\nUser-Agent: %s\r\nHost:%s:%s\r\n\r\n",
		reqStart, cfg.UUID, downloadUA, hostForURL(cfg.Server), cfg.Server.Port)
	if _, err := conn.Write([]byte(req)); err != nil {
		return
	}
	buf := make([]byte, 65536)
	headerDone := false
	headerBuf := make([]byte, 0, 4096)
	for {
		select {
		case <-stop:
			return
		default:
		}
		n, err := conn.Read(buf)
		if n > 0 {
			if headerDone {
				total.Add(int64(n))
			} else {
				headerBuf = append(headerBuf, buf[:n]...)
				if idx := bytes.Index(headerBuf, []byte("\r\n\r\n")); idx >= 0 {
					status := parseHTTPStatus(headerBuf)
					if status != 200 {
						firstErr.set(fmt.Errorf("download http %d", status))
						return
					}
					bodyN := len(headerBuf) - (idx + 4)
					if bodyN > 0 {
						total.Add(int64(bodyN))
					}
					headerDone = true
					headerBuf = nil
				} else if len(headerBuf) > 1<<20 {
					firstErr.set(errors.New("download response header too large"))
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func runUploadConn(conn net.Conn, cfg TransferConfig, total *atomic.Int64, firstErr *errBox, stop <-chan struct{}) {
	header := fmt.Sprintf("POST /speed/doAnalsLoad.do HTTP/1.1\r\nConnection: close\r\nCache-Control: no-cache\r\nCharset: UTF-8\r\nKey: %s\r\nContent-Type: multipart/form-data;boundary=00content0boundary00\r\nUser-Agent: %s\r\nHost: %s:%s\r\nAccept-Encoding: gzip\r\nContent-Length: 900000000\r\n\r\n--00content0boundary00\r\nContent-Disposition: form-data; name=\"upload\";filename=\"SPEED_%s\"\r\n\r\n",
		cfg.UUID, uploadUA, hostForURL(cfg.Server), cfg.Server.Port, time.Now().Format("20060102_150405_000"))
	n, err := conn.Write([]byte(header))
	if n > 0 {
		total.Add(int64(n))
	}
	if err != nil {
		return
	}
	payload := make([]byte, 16384)
	if _, err := rand.Read(payload); err != nil {
		return
	}
	for {
		select {
		case <-stop:
			return
		default:
		}
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		n, err := conn.Write(payload)
		if n > 0 {
			total.Add(int64(n))
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				firstErr.set(errors.New("upload write timeout"))
			}
			return
		}
	}
}

func parseHTTPStatus(header []byte) int {
	line := header
	if idx := bytes.IndexByte(header, '\n'); idx >= 0 {
		line = header[:idx]
	}
	fields := bytes.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	code, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		return 0
	}
	return code
}

func bytesToMbps(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) * 8 / 1e6 / d.Seconds()
}

func averageTop(samples []float64, n int) float64 {
	if len(samples) == 0 || n <= 0 {
		return 0
	}
	cp := append([]float64(nil), samples...)
	sort.Sort(sort.Reverse(sort.Float64Slice(cp)))
	if n > len(cp) {
		n = len(cp)
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += cp[i]
	}
	return sum / float64(n)
}

func maxFloat(samples []float64) float64 {
	m := 0.0
	for _, v := range samples {
		if v > m {
			m = v
		}
	}
	return m
}
