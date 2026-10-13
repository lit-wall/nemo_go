package utils

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hanc00l/nemo_go/v3/pkg/conf"
	"github.com/hanc00l/nemo_go/v3/pkg/logging"
	"golang.org/x/net/proxy"
)

var Socks5Proxy string

// ParseHost 将http://a.b.c:80/这种url去除不相关的字符，返回主机名
func ParseHost(u string) string {
	// url.Parse必须是完整的schema://host:port格式才能解析，比如http://example.org:8080
	// url.Parse支持对ipv6完整URL的解析
	// 返回的hostname为example.org（不带端口）
	p, err := url.Parse(u)
	if err == nil && p.Host != "" {
		return p.Hostname()
	}

	if _, host, _ := ParseHostUrl(u); len(host) > 0 {
		return host
	}
	// 其它格式处理：
	host := strings.ReplaceAll(u, "https://", "")
	host = strings.ReplaceAll(host, "http://", "")
	host = strings.ReplaceAll(host, "/", "")
	host = strings.Split(host, ":")[0]

	return host
}

// ParseHostPort 将http://a.b.c:80/这种url去除不相关的字符，返回主机名,端口号
func ParseHostPort(u string) (string, int) {
	// url.Parse必须是完整的schema://host:port格式才能解析，比如http://example.org:8080
	// url.Parse支持对ipv6完整URL的解析
	// 返回的hostname为example.org（不带端口）
	p, err := url.Parse(u)
	if err == nil && p.Host != "" {
		port, _ := strconv.Atoi(p.Port())
		return p.Hostname(), port
	}

	if _, host, port := ParseHostUrl(u); len(host) > 0 && port > 0 {
		return host, port
	}
	// 其它格式处理：
	url := strings.ReplaceAll(u, "https://", "")
	url = strings.ReplaceAll(url, "http://", "")
	url = strings.ReplaceAll(url, "/", "")
	datas := strings.Split(url, ":")
	var host string
	var port int
	if len(datas) >= 1 {
		host = datas[0]
		if len(datas) >= 2 {
			port, _ = strconv.Atoi(datas[1])
		} else {
			if strings.HasPrefix(u, "https://") {
				port = 443
			} else {
				port = 80
			}
		}
	}
	return host, port
}

func CheckDomain(domain string) bool {
	domainPattern := `^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$`
	reg := regexp.MustCompile(strings.TrimSpace(domainPattern))
	return reg.MatchString(domain)
}

// GetFaviconSuffixUrl 获取favicon文件的后缀名称
func GetFaviconSuffixUrl(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	suffixes := strings.Split(p.Path, ".")
	if len(suffixes) < 2 {
		return ""
	}
	fileSuffix := strings.ToLower(suffixes[len(suffixes)-1])
	if !in(fileSuffix, []string{"ico", "gif", "jpg", "jpeg", "gif", "png", "bmp"}) {
		return ""
	}
	return fileSuffix
}

// GetFaviconSuffixUrlFile 获取favicon文件名
func GetFaviconSuffixUrlFile(u string) string {
	// 解析URL
	parsedURL, err := url.Parse(u)
	if err != nil {
		return ""
	}
	path := parsedURL.Path
	return filepath.Base(path)
}

func in(target string, strArray []string) bool {
	for _, element := range strArray {
		if target == element {
			return true
		}
	}
	return false
}

func WrapperTCP(network, address string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return WrapperTCPWithSocks5(network, address, d)
}

func WrapperTCPWithSocks5(network, address string, forward *net.Dialer) (net.Conn, error) {
	if proxyServer := conf.GetProxyConfig(); proxyServer != "" {
		uri, err := url.Parse(proxyServer)
		if err == nil {
			dial, err := proxy.FromURL(uri, forward)
			if err != nil {
				return nil, err
			}
			conn, err := dial.Dial(network, address)
			if err != nil {
				return nil, err
			}
			return conn, nil
		}
	}
	return forward.Dial(network, address)
}

// GetProtocol 检测URL协议
func GetProtocol(host string, Timeout int64) (protocol string) {
	protocol = "http"
	if strings.HasSuffix(host, ":443") {
		protocol = "https"
		return
	}
	tcpConn, err := WrapperTCP("tcp", host, time.Duration(Timeout)*time.Second)
	if err != nil {
		return
	}
	conn := tls.Client(tcpConn, &tls.Config{InsecureSkipVerify: true})
	defer func() {
		if conn != nil {
			defer func() {
				if err := recover(); err != nil {
					fmt.Println(err)
				}
			}()
			conn.Close()
		}
	}()
	conn.SetDeadline(time.Now().Add(time.Duration(Timeout) * time.Second))
	err = conn.Handshake()
	if err == nil || strings.Contains(err.Error(), "handshake failure") {
		protocol = "https"
	}
	return protocol
}

// ParseHostUrl ipv4/v6地址格式识别的通用函数，用于识别ipv4,ipv4:port,ipv6,[ipv6],[ipv6]:port的形式
func ParseHostUrl(u string) (isIpv6 bool, ip string, port int) {
	// ipv4
	if CheckIPV4(u) {
		ip = u
		return
	}
	// ipv6地址 2400:dd01:103a:4041::10
	if CheckIPV6(u) {
		isIpv6 = true
		ip = u
		return
	}
	//ipv6地址：[2400:dd01:103a:4041::101]:443
	if strings.Index(u, "[") >= 0 && strings.Index(u, "]") >= 0 {
		ipv6Re := regexp.MustCompile(`\[(.*?)\]`)
		m := ipv6Re.FindStringSubmatch(u)
		if len(m) == 2 && CheckIPV6(m[1]) {
			isIpv6 = true
			ip = m[1]
			portRe := regexp.MustCompile(`\[.*?\]:(\d{1,5})`)
			n := portRe.FindStringSubmatch(u)
			if len(n) == 2 {
				port, _ = strconv.Atoi(n[1])
			}
			return
		}
	}
	// ipv4:port  192.168.1.1:443
	if strings.Index(u, ":") > 0 {
		ipp := strings.Split(u, ":")
		if len(ipp) == 2 {
			if CheckIPV4(ipp[0]) {
				ip = ipp[0]
				port, _ = strconv.Atoi(ipp[1])
			}
			return
		}
	}
	return
}

// GetProxyHttpClient 获取代理的http client
func GetProxyHttpClient(isProxy bool) *http.Client {
	var transport *http.Transport
	transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	if isProxy {
		if proxyConfig := conf.GetProxyConfig(); proxyConfig != "" {
			proxyURL, parseErr := url.Parse(proxyConfig)
			if parseErr != nil {
				logging.RuntimeLog.Warningf("proxyConfig config fail:%v,skip proxyConfig!", parseErr)
				logging.CLILog.Warningf("proxyConfig config fail:%v,skip proxyConfig!", parseErr)
			} else {
				transport = &http.Transport{
					Proxy:           http.ProxyURL(proxyURL),
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
			}
		} else {
			logging.RuntimeLog.Warning("get proxyConfig config fail or disabled by worker,skip proxyConfig!")
			logging.CLILog.Warning("get proxyConfig config fail or disabled by worker,skip proxyConfig!")
		}
	}
	httpClient := &http.Client{
		Transport: transport,
	}
	return httpClient
}

// GetTaskProxyHttpClient 获取任务级代理的 http client（匿名性加固，V2.0.8 P2.5）。
// 优先级：任务指定代理（proxy 非空，MCP 建任务时按出口角色注入）> 全局代理开关
// （isProxy + worker 配置）> 直连。任务代理解析失败时回退全局策略并告警。
func GetTaskProxyHttpClient(proxy string, isProxy bool) *http.Client {
	proxy = strings.TrimSpace(proxy)
	if proxy != "" {
		if proxyURL, err := url.Parse(proxy); err == nil {
			return &http.Client{
				Transport: &http.Transport{
					Proxy:           http.ProxyURL(proxyURL),
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				},
			}
		}
		logging.RuntimeLog.Warningf("task proxy parse fail: %v, fallback to global policy", err)
		logging.CLILog.Warningf("task proxy parse fail: %v, fallback to global policy", err)
	}
	return GetProxyHttpClient(isProxy)
}

func MergeTarget(targets ...string) string {
	targetSet := make(map[string]struct{})

	for _, target := range targets {
		if target == "" {
			continue
		}

		// 避免不必要的Split操作：如果字符串不包含逗号，直接添加
		if !strings.Contains(target, ",") {
			targetSet[target] = struct{}{}
			continue
		}

		// 对于包含逗号的字符串才进行Split
		for _, t := range strings.Split(target, ",") {
			if t != "" { // 避免空字符串
				targetSet[t] = struct{}{}
			}
		}
	}

	if len(targetSet) == 0 {
		return ""
	}

	// 直接在这里构建结果，避免额外的函数调用
	result := make([]string, 0, len(targetSet))
	for k := range targetSet {
		result = append(result, k)
	}
	return strings.Join(result, ",")
}

func UnmarshalTargetMap(content string) (targetMap map[string]string) {
	_ = json.Unmarshal([]byte(content), &targetMap)
	return
}

func MarshalTargetMap(targetMap map[string]string) string {
	content, _ := json.Marshal(targetMap)
	return string(content)
}

func ReturnListTypedTargetMap(typeKey string, targets []string) (targetMap map[string]string) {
	targetMap = make(map[string]string)
	targetMap[typeKey] = strings.Join(targets, ",")
	return

}

func ReturnTypedTargetMap(typeKey string, target string) (targetMap map[string]string) {
	targetMap = make(map[string]string)
	targetMap[typeKey] = target
	return

}
