# 全球网测测速逻辑复刻（Go）

本项目是对「全球网测」Android 客户端（`com.cnspeedtest.globalspeed`，4.4.8 / versionCode 40408）测速逻辑的 Go 复刻。

模块路径：`github.com/ankikong/speedtest`

APK 本身未包含在仓库中。逆向过程使用了 frida-dexdump 导出内存 dex、jadx 反编译，并解密了 App 运行时日志做交叉验证。

## 核心逻辑

### 1. 获取外网 IP、归属地和运营商

```text
GET https://dlcv2.cnspeedtest.cn:8443/dataServer/getIpLocSP.php
```

响应格式：

```text
ip|["国家","省","市","","运营商"]|运营商ID|运营商|端口
```

示例：

```text
183.34.119.57|["中国","广东","江门","","电信"]|3|电信|53372
```

### 2. 获取匹配的测速服务器

```text
GET https://dlcv2.cnspeedtest.cn:8443/dataServer/mobilematch_many.php
  ?ip=<外网IP>
  &network=4
  &province=<省>
  &city=<市>
  &wifioper=<运营商>
  &mobileoperid=0
  &ipv6=0
  &model=Android
  &pkg=com.cnspeedtest.globalspeed
```

参数说明：

| 参数 | 说明 |
| --- | --- |
| `network` | `4` 表示 WiFi，`5` 表示移动网络 |
| `ipv6` | `1` 请求 IPv6 服务器，`0` 为 IPv4 |
| `mobileoperid` | SIM 卡运营商代码，无 SIM 时为 `0` |
| `wifioper` | WiFi 出口运营商描述，如 `电信` |

响应为 JSON 数组：

```json
[
  {
    "hostid": "1101",
    "pname": "广东",
    "city": "广州",
    "port": "65499",
    "hostname": "广州电信",
    "hostip": "61.144.6.18"
  }
]
```

### 3. 选择服务器

原 App 不是按时延排序，而是：

1. 按接口返回顺序遍历服务器；
2. 对每台服务器向 `hostip:port` 发起 2 次 TCP 握手；
3. 第一台握手成功且 `rttAvg > 0.1ms` 的服务器即为选中服务器；
4. 后续服务器不再测试。

因此实际选择结果是「接口优先级顺序 + 第一台可达」，不是「最低时延」。

手动选服时直接用 `hostid` 对应的服务器，不再做可达性测试。

### 4. 发起测速

#### 4.1 Ping

```text
ping -i 0.2 -s 64 -c 10 -W 3 <server_ip>
```

部分设备不支持快速 Ping 时改为 5 次；本项目在 ICMP 不可用时回退为 TCP 握手 Ping。

#### 4.2 排队并获取会话 UUID

```text
GET http://<ip>:<port>/speed/dovalid
  ?key=
  &flag=true
  &bandwidth=200
  &model=Android
  &imei=<设备标识>
  &time=<unix 秒>
  &app=globalspeed
  &token=<签名>
  &pkg=com.cnspeedtest.globalspeed
```

签名算法：

```text
h1 = md5("model=Android&imei=" + imei)
h2 = md5("stime=" + unixSeconds + "&band=" + bandwidth + "&rand=12345555")
token = md5(h1 + h2)
```

响应格式为 `1-<uuid>`，取第 3 个字符起作为后续下载、上传使用的会话 UUID。

#### 4.3 下行测速

```text
GET http://<ip>:<port>/speed/File(1G).dl?r=<unix 秒>&key=<uuid>
```

请求头与 App 保持一致，注意下行请求的 `Host:` 后没有空格：

```text
Accept: */*
Connection: close
User-Agent: Mozilla/5.0 (Windows NT 10.0; WOW64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/100.0.4896.60 Safari/537.36
Host:<ip>:<port>
```

#### 4.4 上行测速

```text
POST http://<ip>:<port>/speed/doAnalsLoad.do
```

请求头：

```text
Connection: close
Cache-Control: no-cache
Charset: UTF-8
Key: <uuid>
Content-Type: multipart/form-data;boundary=00content0boundary00
User-Agent: Dalvik/1.6.0 (Linux; U; Android 4.2.2; GT-I9505 Build/JDQ39)
Host: <ip>:<port>
Accept-Encoding: gzip
Content-Length: 900000000
```

随后持续写入随机字节，直到采样结束。

#### 4.5 释放会话

```text
POST http://<ip>:<port>/speed/dovalid?key=<uuid>
```

请求体为空。

### 5. 采样与统计

- 采样间隔：500ms
- 预热采样：4 个点
- 正式采样：16 个点
- 单向测试约 10 秒，最长等待 20 秒
- 速率公式与 App 一致：

```text
首个采样点：bytes * 8 / 1e6 / 0.5
后续采样点：(上次增量 + 本次增量) * 8 / 1e6 / 1.0
```

- 最终平均速率取正式样本的 Top 3（与 Android 15 以下版本一致）

### 6. 线程数

| 网络类型 | 下行线程 | 上行线程 | bandwidth |
| --- | --- | --- | --- |
| WiFi | 8 | 4 | 200 |
| 4G | 10 | 4 | 150 |
| 5G | 16 | 6 | 300 |

`-network 5` 默认按 5G 取 `bandwidth=300`；4G 场景可加 `-bandwidth 150`。

### 7. 本地全量服务器列表

App 还会维护一份全量服务器列表，用于手动选服：

1. `ConfigMD5.php` 返回 `serverlist_encrypt_url`；
2. 下载 `serverlist_encrypt.json`；
3. 文件内容为 hex 编码的 DES-ECB 密文；
4. 密钥由 `Jni.e()` 经 `constDecrypt` 得到，即 `dw!@#$%^`。

解密示例：

```bash
./gspeed -decrypt serverlist.json > serverlist.json.decoded
```

当前版本解密后约 606 台服务器。

## 构建

```bash
git clone git@github.com:ankikong/speedtest.git
cd speedtest
go build -o gspeed .
```

也可以直接安装：

```bash
go install github.com/ankikong/speedtest@latest
```

## 用法

```bash
# 只获取匹配的服务器列表
./gspeed -list

# 只做服务器选择
./gspeed -select-only

# 完整测速
./gspeed

# 完整测速并输出 JSON
./gspeed -json

# 手动指定服务器
./gspeed -server 31791

# 移动网络
./gspeed -network 5

# 解密本地服务器列表
./gspeed -decrypt serverlist.json
```

常用参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-ip` | 自动获取 | 外网 IP |
| `-province` / `-city` | 自动获取 | 归属地 |
| `-wifioper` | 自动获取 | WiFi 运营商 |
| `-mobileoperid` | `0` | SIM 运营商代码 |
| `-network` | `4` | `4`=WiFi，`5`=移动网络 |
| `-ipv6` | `false` | 请求 IPv6 服务器 |
| `-imei` | 随机生成 | 设备标识 |
| `-bandwidth` | 按网络类型 | 排队带宽参数 |
| `-down` / `-up` | 按网络类型 | 上下行线程数 |
| `-points` | `16` | 正式采样点数 |
| `-preheat` | `4` | 预热采样点数 |
| `-interval` | `500ms` | 采样间隔 |
| `-max-wait` | `20s` | 单向测试最长等待 |
| `-timeout` | `5s` | TCP 连接超时 |
| `-ping-count` | `10` | Ping 次数 |
| `-server` | 空 | 手动指定服务器 |
| `-list` | `false` | 只打印服务器列表 |
| `-select-only` | `false` | 只选择服务器 |
| `-enqueue-only` | `false` | 只排队并打印 UUID |
| `-no-dequeue` | `false` | 不释放会话 |
| `-json` | `false` | JSON 输出 |
| `-v` | `false` | 打印请求过程 |

## 输出示例

```text
自动选择 南昌电信 (59.52.20.90:65499)，TCP 握手 19.56 ms
下行: 340.32 Mbps (max 352.44 Mbps, 8 线程)
上行: 69.69 Mbps (max 69.99 Mbps, 4 线程)
Ping: avg 21.45 ms, jitter 0.69 ms, loss 0.00% (icmp)
```

## 目录结构

```text
.
├── api.go        # IP/归属地、服务器匹配、token、dovalid/dequeue
├── server.go     # TCP 测速选服
├── transfer.go   # 多线程上下行与采样
├── ping.go       # ICMP Ping 与 TCP 回退
├── crypto.go     # 本地 serverlist DES 解密
├── main.go       # CLI 编排
└── go.mod
```

## 已知限制

- `dovalid` 对同一出口 IP 有频率限制，短时间重复请求会返回 `-1-`。
- 未实现测速结果上报 `upload_data_std.php`。
- 未实现版本检查、公告、配置自动更新等非测速功能。
- 平均速率默认取 Top 3；Android 15 及以上原 App 会改为全部正式样本平均。
- ICMP Ping 依赖系统 `ping`，不可用时自动回退为 TCP Ping。

## 免责声明

本项目仅用于协议学习和研究，请遵守相关服务条款。
