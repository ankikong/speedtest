package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	httpSpeedServer = "https://dlcv2.cnspeedtest.cn:8443"
	ipLocPath       = "/dataServer/getIpLocSP.php"
	matchPath       = "/dataServer/mobilematch_many.php"
	userAgent       = "Mozilla/5.0 (Linux; Android 11) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/100.0.4896.60 Mobile Safari/537.36"
)

type IPLoc struct {
	IP       string `json:"ip"`
	Country  string `json:"country"`
	Province string `json:"province"`
	City     string `json:"city"`
	Oper     string `json:"oper"`
	OperID   int    `json:"oper_id"`
	Port     string `json:"port"`
}

type Server struct {
	HostID   string `json:"hostid"`
	PName    string `json:"pname"`
	City     string `json:"city"`
	Port     string `json:"port"`
	HostName string `json:"hostname"`
	HostIP   string `json:"hostip"`
	IPv6     bool   `json:"ipv6"`
}

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 8,
		},
	}
}

func httpGet(ctx context.Context, client *http.Client, rawurl string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)), nil
}

func fetchIPLoc(ctx context.Context, client *http.Client) (IPLoc, error) {
	body, err := httpGet(ctx, client, httpSpeedServer+ipLocPath)
	if err != nil {
		return IPLoc{}, err
	}
	parts := strings.Split(body, "|")
	if len(parts) < 5 {
		return IPLoc{}, fmt.Errorf("unexpected getIpLocSP reply: %s", body)
	}
	loc := IPLoc{IP: parts[0]}
	var arr []string
	if err := json.Unmarshal([]byte(parts[1]), &arr); err == nil {
		if len(arr) > 0 {
			loc.Country = arr[0]
		}
		if len(arr) > 1 {
			loc.Province = arr[1]
		}
		if len(arr) > 2 {
			loc.City = arr[2]
		}
		if len(arr) > 4 {
			loc.Oper = arr[4]
		}
	}
	loc.OperID, _ = strconv.Atoi(parts[2])
	if parts[3] != "" {
		loc.Oper = parts[3]
	}
	loc.Port = parts[4]
	return loc, nil
}

func matchServers(ctx context.Context, client *http.Client, ip, province, city, wifiOper, mobileOperID string, network int, ipv6 bool, pkg string) ([]Server, string, error) {
	q := url.Values{}
	q.Set("ip", ip)
	q.Set("network", strconv.Itoa(network))
	q.Set("province", province)
	q.Set("city", city)
	q.Set("wifioper", wifiOper)
	q.Set("mobileoperid", mobileOperID)
	if ipv6 {
		q.Set("ipv6", "1")
	} else {
		q.Set("ipv6", "0")
	}
	q.Set("model", "Android")
	q.Set("pkg", pkg)
	rawurl := httpSpeedServer + matchPath + "?" + q.Encode()
	body, err := httpGet(ctx, client, rawurl)
	if err != nil {
		return nil, rawurl, err
	}
	var servers []Server
	if err := json.Unmarshal([]byte(body), &servers); err != nil {
		return nil, rawurl, fmt.Errorf("decode server list: %w", err)
	}
	for i := range servers {
		servers[i].IPv6 = strings.Contains(servers[i].HostIP, ":")
	}
	if len(servers) == 0 {
		return nil, rawurl, errors.New("empty server list")
	}
	return servers, rawurl, nil
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func makeToken(model, imei, stime string, bandwidth int) string {
	first := md5Hex("model=" + model + "&imei=" + imei)
	second := md5Hex("stime=" + stime + "&band=" + strconv.Itoa(bandwidth) + "&rand=12345555")
	return md5Hex(first + second)
}

func enqueue(ctx context.Context, client *http.Client, srv Server, imei, app, pkg string, bandwidth int) (string, string, error) {
	stime := strconv.FormatInt(time.Now().Unix(), 10)
	token := makeToken("Android", imei, stime, bandwidth)
	host := srv.HostIP
	if srv.IPv6 {
		host = "[" + srv.HostIP + "]"
	}
	rawurl := fmt.Sprintf("http://%s:%s/speed/dovalid?key=&flag=true&bandwidth=%d&model=Android&imei=%s&time=%s&app=%s&token=%s&pkg=%s",
		host, srv.Port, bandwidth, url.QueryEscape(imei), stime, url.QueryEscape(app), token, url.QueryEscape(pkg))
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		body, err := httpGet(ctx, client, rawurl)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		switch {
		case body == "":
			return "", rawurl, errors.New("empty dovalid reply")
		case strings.HasPrefix(body, "-1"):
			return "", rawurl, errors.New("dovalid rejected (-1): invalid parameters or too many queued requests")
		case strings.HasPrefix(body, "0"):
			return "", rawurl, errors.New("dovalid refused (0)")
		case strings.HasPrefix(body, "2"):
			return "", rawurl, errors.New("dovalid busy (2)")
		case len(body) < 2:
			return "", rawurl, fmt.Errorf("unexpected dovalid reply: %s", body)
		default:
			return body[2:], rawurl, nil
		}
	}
	return "", rawurl, lastErr
}

func dequeue(ctx context.Context, client *http.Client, srv Server, uuid string) error {
	host := srv.HostIP
	if srv.IPv6 {
		host = "[" + srv.HostIP + "]"
	}
	rawurl := fmt.Sprintf("http://%s:%s/speed/dovalid?key=%s", host, srv.Port, url.QueryEscape(uuid))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawurl, strings.NewReader(""))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dequeue http %d", resp.StatusCode)
	}
	return nil
}

func provinceFullName(s string) string {
	if len([]rune(s)) >= 4 {
		return s
	}
	switch s {
	case "北京", "天津", "上海", "重庆":
		return s + "市"
	case "辽宁", "湖北", "四川", "山西", "浙江", "黑龙江", "江苏", "陕西", "贵州", "山东", "福建", "安徽", "吉林", "河北", "青海", "云南", "江西", "海南", "甘肃", "湖南", "河南", "广东":
		return s + "省"
	case "广西":
		return "广西壮族自治区"
	case "宁夏":
		return "宁夏回族自治区"
	case "新疆":
		return "新疆维吾尔自治区"
	case "内蒙古":
		return "内蒙古自治区"
	case "西藏":
		return "西藏自治区"
	}
	return s
}

func cityFullName(s string) string {
	if s == "" || strings.HasSuffix(s, "市") || strings.HasSuffix(s, "州") || strings.HasSuffix(s, "盟") || strings.HasSuffix(s, "地区") {
		return s
	}
	return s + "市"
}
