package onlineapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/hanc00l/nemo_go/v3/pkg/core"
	"github.com/hanc00l/nemo_go/v3/pkg/task/execute"
	"github.com/hanc00l/nemo_go/v3/pkg/utils"
)

// Crtsh 证书透明度日志搜索（crt.sh），免费无需 API Key。
// 搜索原理：匹配 CA 证书日志中注册的域名（common_name / name_value），
// 适合零种子场景下的子域名枚举与域名特征发现。
type Crtsh struct {
	IsProxy bool
}

func (c *Crtsh) GetRequiredResources() (re []core.RequiredResource) {
	// crt.sh 无需本地资源文件
	return
}

func (c *Crtsh) GetSyntaxMap() (syntax map[SyntaxType]string) {
	syntax = make(map[SyntaxType]string)
	syntax[And] = " "
	syntax[Or] = " "
	syntax[Equal] = ""
	syntax[Not] = "!"
	syntax[After] = ""
	syntax[Title] = ""
	syntax[Body] = ""
	return
}

func (c *Crtsh) MakeSearchSyntax(syntax map[SyntaxType]string, condition SyntaxType, checkMod SyntaxType, value string) string {
	return value
}

func (c *Crtsh) GetQueryString(domain string, config execute.OnlineAPIConfig, filterKeyword map[string]struct{}) (query string) {
	if config.SearchByKeyWord {
		// 关键词模式：crt.sh 默认子串匹配，直接传原文。
		// 用户输入通配符前缀（* 或 %）时转为 crt.sh 的 % 格式。
		target := config.Target
		if strings.HasPrefix(target, "*") || strings.HasPrefix(target, "%") {
			trimmed := strings.TrimLeft(target, "*%")
			query = "%" + trimmed
		} else {
			query = target
		}
	} else {
		if utils.CheckIPOrSubnet(domain) {
			query = domain
		} else {
			query = fmt.Sprintf("%%.%s", domain)
		}
	}
	return
}
// appendRegionFilter crt.sh 无原生 region 过滤。
// 通过 TLD 粗略判断（.cn → 中国），其余按 oversea 处理。
func (c *Crtsh) appendRegionFilter(query string, config execute.OnlineAPIConfig) string {
	// crt.sh 不支持 region 表达式，返回原 query（region 过滤在结果解析阶段做）
	return query
}

// crtshEntry crt.sh API 返回的单条证书记录
type crtshEntry struct {
	CommonName string `json:"common_name"`
	NameValue  string `json:"name_value"`
	IssuerName string `json:"issuer_name"`
	ExpireDate string `json:"expire_date"`
	NotBefore  string `json:"not_before"`
	NotAfter   string `json:"not_after"`
	Serial     string `json:"serial_number"`
}

// Run 调用 crt.sh API 搜索证书，返回解析后的资产列表
func (c *Crtsh) Run(query string, apiKey string, pageIndex int, pageSize int, config execute.OnlineAPIConfig) (pageResult []OnlineSearchResult, sizeTotal int, err error) {
	// crt.sh API: https://crt.sh/?q=QUERY&output=json
	// count 参数限制返回数量（crt.sh 最大约 1000 条/请求）
	limit := pageSize
	if limit <= 0 || limit > 1000 {
		limit = config.SearchLimitCount
		if limit <= 0 || limit > 1000 {
			limit = 500
		}
	}

	params := url.Values{}
	params.Add("q", query)
	params.Add("output", "json")
	params.Add("count", fmt.Sprintf("%d", limit))

	requestURL := "https://crt.sh/?" + params.Encode()
	request, err := http.NewRequest(http.MethodGet, requestURL, nil)
	if err != nil {
		return
	}
	request.Header.Set("User-Agent", "Mozilla/5.0 (compatible; NemoBot/3.0)")
	request.Header.Set("Accept", "application/json")

	// 任务级代理优先（MCP 按出口角色注入），避免容器出口 IP 直接暴露给 crt.sh
	resp, err := utils.GetTaskProxyHttpClient(config.Proxy, c.IsProxy).Do(request)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("crt.sh 返回 HTTP %d", resp.StatusCode)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	// crt.sh 返回 JSON 数组
	var entries []crtshEntry
	if err = json.Unmarshal(body, &entries); err != nil {
		// crt.sh 偶尔返回带 BOM 或空数组，容错处理
		trimmed := strings.TrimLeft(string(body), "\xef\xbb\xbf ")
		if trimmed == "" || trimmed == "[]" {
			pageResult = []OnlineSearchResult{}
			sizeTotal = 0
			return
		}
		err = json.Unmarshal([]byte(trimmed), &entries)
		if err != nil {
			err = fmt.Errorf("解析 crt.sh 响应失败: %w (body[:100]=%s)", err, string(body[:min(100, len(body))]))
			return
		}
	}

	// 提取域名（从 common_name / name_value），去重
	seen := make(map[string]struct{})
	for _, entry := range entries {
		names := c.extractNames(entry)
		for _, name := range names {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}

			// region 后过滤（通过 TLD 判断）
			if !c.passRegionFilter(name, config) {
				continue
			}

			// 过滤泛域名占位符
			if strings.Contains(name, "*") {
				continue
			}

			fsr := OnlineSearchResult{
				Domain: name,
				Host:   name,
				Cert:   entry.IssuerName,
				Source: "crtsh",
			}
			pageResult = append(pageResult, fsr)
		}
	}

	// 按 domain 排序保证确定性
	sort.Slice(pageResult, func(i, j int) bool {
		return pageResult[i].Domain < pageResult[j].Domain
	})

	// 应用 limit 截断
	if config.SearchLimitCount > 0 && len(pageResult) > config.SearchLimitCount {
		pageResult = pageResult[:config.SearchLimitCount]
	}
	sizeTotal = len(pageResult)
	return
}

// extractNames 从 crt.sh 证书条目中提取域名列表
func (c *Crtsh) extractNames(entry crtshEntry) []string {
	var names []string
	// common_name 可能是逗号分隔的多值
	if entry.CommonName != "" {
		for _, cn := range strings.Split(entry.CommonName, ",") {
			name := strings.TrimSpace(cn)
			if name != "" && name != "." {
				names = append(names, name)
			}
		}
	}
	// name_value 是换行分隔的 SAN 列表
	if entry.NameValue != "" {
		for _, nv := range strings.Split(entry.NameValue, "\n") {
			name := strings.TrimSpace(nv)
			if name != "" && name != "." {
				names = append(names, name)
			}
		}
	}
	return names
}

// passRegionFilter 基于 TLD 的粗略 region 过滤
func (c *Crtsh) passRegionFilter(domain string, config execute.OnlineAPIConfig) bool {
	switch config.RegionFilter {
	case "mainland":
		return isChinaTLD(domain) && !isChinaSpecialTLD(domain)
	case "china":
		return isChinaTLD(domain)
	case "oversea":
		return !isChinaTLD(domain)
	case "all", "":
		return true
	default:
		return true
	}
}

var chinaTLDs = map[string]bool{
	".cn": true, ".中国": true,
}

var chinaSpecialTLDs = map[string]bool{
	".hk": true, ".tw": true, ".mo": true,
}

func isChinaTLD(domain string) bool {
	lower := strings.ToLower(domain)
	for tld := range chinaTLDs {
		if strings.HasSuffix(lower, tld) {
			return true
		}
	}
	return false
}

func isChinaSpecialTLD(domain string) bool {
	lower := strings.ToLower(domain)
	for tld := range chinaSpecialTLDs {
		if strings.HasSuffix(lower, tld) {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
