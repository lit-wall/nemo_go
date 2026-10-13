package onlineapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/hanc00l/nemo_go/v3/pkg/core"
	"github.com/hanc00l/nemo_go/v3/pkg/resource"
	"github.com/hanc00l/nemo_go/v3/pkg/task/execute"
	"github.com/hanc00l/nemo_go/v3/pkg/utils"
)

type FOFA struct {
	IsProxy bool
}

func (f *FOFA) GetRequiredResources() (re []core.RequiredResource) {
	//CDN
	re = append(re, core.RequiredResource{
		Category: resource.Geolite2Category,
		Name:     "GeoLite2-ASN.mmdb",
	})
	//ip location:
	re = append(re, core.RequiredResource{
		Category: resource.QqwryCategory,
		Name:     "qqwry.dat",
	})
	re = append(re, core.RequiredResource{
		Category: resource.Zxipv6wryCategory,
		Name:     "ipv6wry.db",
	})
	return
}

func (f *FOFA) MakeSearchSyntax(syntax map[SyntaxType]string, condition SyntaxType, checkMod SyntaxType, value string) string {
	// title="百度"
	return fmt.Sprintf("%s%s\"%s\"", syntax[checkMod], syntax[condition], value)
}

func (f *FOFA) GetSyntaxMap() (syntax map[SyntaxType]string) {
	syntax = make(map[SyntaxType]string)
	syntax[And] = "&&"
	syntax[Or] = "||"
	syntax[Equal] = "="
	syntax[Not] = "!="
	syntax[After] = "after"
	syntax[Title] = "title"
	syntax[Body] = "body"

	return
}

func (f *FOFA) GetQueryString(domain string, config execute.OnlineAPIConfig, filterKeyword map[string]struct{}) (query string) {
	if config.SearchByKeyWord {
		query = config.Target
	} else {
		if utils.CheckIPOrSubnet(domain) {
			query = fmt.Sprintf("ip=\"%s\"", domain)
		} else {
			query = fmt.Sprintf("domain=\"%s\"", domain)
		}
	}
	if words := f.getFilterTitleKeyword(filterKeyword); len(words) > 0 {
		query = fmt.Sprintf("(%s) && (%s)", query, words)
	}
	query = f.appendRegionFilter(query, config)
	if len(config.SearchStartTime) > 0 {
		query = fmt.Sprintf("(%s) && after=\"%s\"", query, config.SearchStartTime)
	}

	return
}

// appendRegionFilter 区域过滤（魔改：MCP 自动发现按 region 路由）。
// RegionFilter 非空时优先；为空则回退旧版布尔开关（Web UI 任务兼容）。
func (f *FOFA) appendRegionFilter(query string, config execute.OnlineAPIConfig) string {
	switch config.RegionFilter {
	case "mainland":
		return fmt.Sprintf("(%s) && (country=\"CN\" && region!=\"HK\" && region!=\"TW\" && region!=\"MO\")", query)
	case "china":
		return fmt.Sprintf("(%s) && (country=\"CN\")", query)
	case "oversea":
		return fmt.Sprintf("(%s) && (country!=\"CN\")", query)
	case "all", "":
		if config.IsIgnoreChinaOther {
			return fmt.Sprintf("(%s) && (country=\"CN\" && region!=\"HK\" && region!=\"TW\"  && region!=\"MO\")", query)
		}
		if config.IsIgnoreOutsideChina {
			return fmt.Sprintf("(%s) && (country=\"CN\")", query)
		}
		return query
	default:
		return query
	}
}

func (f *FOFA) getFilterTitleKeyword(filterKeyword map[string]struct{}) string {
	var words []string
	for k := range filterKeyword {
		words = append(words, fmt.Sprintf("body!=\"%s\"", k))
	}

	return strings.Join(words, " && ")
}

func (f *FOFA) Run(query string, apiKey string, pageIndex int, pageSize int, config execute.OnlineAPIConfig) (pageResult []OnlineSearchResult, sizeTotal int, err error) {
	fields := "domain,host,ip,port,title,country,city,server,banner,protocol,cert"
	//fofa api请求只要求key（不再需要email）
	var key string
	arr := strings.Split(apiKey, ":")
	if len(arr) == 2 {
		key = arr[1]
	} else {
		key = apiKey
	}
	request, err := http.NewRequest(http.MethodGet, "https://fofa.info/api/v1/search/all", nil)
	if err != nil {
		return
	}
	params := make(url.Values)
	params.Add("key", key)
	params.Add("qbase64", base64.StdEncoding.EncodeToString([]byte(query)))
	params.Add("fields", fields)
	params.Add("page", strconv.Itoa(pageIndex))
	params.Add("size", strconv.Itoa(pageSize))
	request.URL.RawQuery = params.Encode()
	// 任务级代理优先（MCP 按出口角色注入），避免容器出口 IP 直接暴露给 fofa
	resp, err := utils.GetTaskProxyHttpClient(config.Proxy, f.IsProxy).Do(request)
	if err != nil {
		return
	}
	content, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	pageResult, sizeTotal, err = f.parseFofaSearchResult(content)

	return
}

func (f *FOFA) parseFofaSearchResult(queryResult []byte) (result []OnlineSearchResult, sizeTotal int, err error) {
	r := fofaQueryResult{}
	err = json.Unmarshal(queryResult, &r)
	if err != nil {
		return
	}
	if r.IsError {
		err = errors.New(r.ErrorMessage)
		return
	}
	sizeTotal = r.Size
	for _, line := range r.Results {
		fsr := OnlineSearchResult{
			Domain: line[0], Host: line[1], IP: line[2], Port: line[3], Title: line[4],
			Country: line[5], City: line[6], Server: line[7], Banner: line[8], Service: line[9], Cert: line[10],
			Source: "fofa",
		}
		lowerBanner := strings.ToLower(fsr.Banner)
		//过滤部份无意义的banner
		if strings.HasPrefix(fsr.Banner, "HTTP/") || strings.HasPrefix(lowerBanner, "<html>") || strings.HasPrefix(lowerBanner, "<!doctype html") || strings.HasPrefix(lowerBanner, "<?xml version") {
			fsr.Banner = ""
		}
		//过滤所有的\x00 \x15等不可见字符
		re := regexp.MustCompile("\\\\x[0-9a-f]{2}")
		fsr.Banner = re.ReplaceAllString(fsr.Banner, "")
		result = append(result, fsr)
	}

	return
}
