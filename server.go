package main

import (
	"errors"
	"fmt"
	"net"
	"time"
)

type ServerChoice struct {
	Server Server
	RTT    time.Duration
	Auto   bool
}

func tcpPing(ip, port string, attempts int, timeout time.Duration) (time.Duration, int) {
	addr := net.JoinHostPort(ip, port)
	var total time.Duration
	okCount := 0
	for i := 0; i < attempts; i++ {
		start := time.Now()
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			continue
		}
		total += time.Since(start)
		okCount++
		conn.Close()
	}
	if okCount == 0 {
		return 0, 0
	}
	return total / time.Duration(okCount), okCount
}

func selectServer(servers []Server, manual string, timeout time.Duration) (ServerChoice, error) {
	if manual != "" {
		for _, s := range servers {
			if s.HostID == manual || s.HostIP == manual || s.HostName == manual {
				return ServerChoice{Server: s, Auto: false}, nil
			}
		}
		return ServerChoice{}, fmt.Errorf("server %q not found in matched list", manual)
	}
	for _, s := range servers {
		rtt, ok := tcpPing(s.HostIP, s.Port, 2, timeout)
		if ok > 0 && rtt > 100*time.Microsecond {
			return ServerChoice{Server: s, RTT: rtt, Auto: true}, nil
		}
	}
	return ServerChoice{}, errors.New("no reachable speed test server")
}
