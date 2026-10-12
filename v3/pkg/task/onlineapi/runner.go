package onlineapi

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/hanc00l/nemo_go/v3/pkg/conf"
	"github.com/hanc00l/nemo_go/v3/pkg/core"
	"github.com/hanc00l/nemo_go/v3/pkg/logging"
	"github.com/hanc00l/nemo_go/v3/pkg/task/execute"
	"github.com/hanc00l/nemo_go/v3/pkg/utils"
)

// Executor 网络空间资产搜索引擎interface
type Executor interface {
	GetRequiredResources() (re []core.RequiredResource)
	GetSyntaxMap() (syntax map[SyntaxType]string)
	MakeSearchSyntax(syntax map[SyntaxType]string, condition SyntaxType, checkMod SyntaxType, value string) string
	GetQueryString(domain string, config execute.OnlineAPIConfig, filterKeyword map[string]struct{}) (query string)
	Run(query string, apiKey string, pageIndex int, pageSize int, config execute.OnlineAPIConfig) (pageResult []OnlineSearchResult, sizeTotal int, err error)
}

// ExecutorQueryAndLookup 用于icp、whois等api的interface
type ExecutorQueryAndLookup interface {
	Run(domain string, apiKey string) (result QueryDataResult)
}

type SyntaxType int

type APIKey struct {
	apiName     string
	apiKey      string
	apiKeyInUse string
}

const (
	And SyntaxType = iota
	Or
	Equal
	Not
	After
	Title
	Body
)

func NewExecutor(executeName string, isProxy bool) (Executor, APIKey) {
	executorMap := map[string]Executor{
		"fofa":   &FOFA{IsProxy: isProxy},
		"hunter": &Hunter{IsProxy: isProxy},
		"quake":  &Quake{IsProxy: isProxy},
		"crtsh":  &Crtsh{IsProxy: isProxy},
	}
	apiKeyMap := map[string]APIKey{
		"fofa":   {apiName: "fofa", apiKey: conf.GlobalWorkerConfig().API.Fofa.Key},
		"hunter": {apiName: "hunter", apiKey: conf.GlobalWorkerConfig().API.Hunter.Key},
		"quake":  {apiName: "quake", apiKey: conf.GlobalWorkerConfig().API.Quake.Key},
		"crtsh":  {apiName: "crtsh", apiKey: ""},
	}
	return executorMap[executeName], apiKeyMap[executeName]
}

func Do(taskInfo execute.ExecutorTaskInfo) (result []OnlineSearchResult) {
	config, ok := taskInfo.OnlineAPI[taskInfo.Executor]
	if !ok {
		logging.RuntimeLog.Errorf("子任务的executor配置不存在：%s", taskInfo.Executor)
		return
	}
	executor, apiKey := NewExecutor(taskInfo.Executor, taskInfo.IsProxy)
	if executor == nil {
		logging.RuntimeLog.Errorf("子任务的executor不存在：%s", taskInfo.Executor)
		return
	}
	re := executor.GetRequiredResources()
	if len(re) > 0 {
		err := core.CheckRequiredResource(re, false)
		if err != nil {
			logging.RuntimeLog.Errorf("任务资源检查和请求失败:%s", err.Error())
			return
		}
	}
	var targets string
	if config.SearchByKeyWord {
		// 关键词（dork）模式：查询表达式整串透传，不进 MergeTarget 的逗号切分/集合重排（防表达式被拆断或乱序）
		// 优先读 unit 键（MCP keyword 模式），回退 rootDomain（旧任务兼容）
		targets = strings.TrimSpace(taskInfo.TargetMap[execute.TargetUnit])
		if targets == "" {
			targets = strings.TrimSpace(taskInfo.TargetMap[execute.TargetRootDomain])
		}
	} else {
		targets = utils.MergeTarget(taskInfo.TargetMap[execute.TargetIp], taskInfo.TargetMap[execute.TargetRootDomain])
	}
	filterKeyword := loadFilterKeyword()
	config.Target = targets
	var lines []string
	if config.SearchByKeyWord {
		lines = []string{strings.TrimSpace(targets)}
	} else {
		lines = strings.Split(targets, ",")
	}
	for _, line := range lines {
		domain := strings.TrimSpace(line)
		if domain == "" {
			continue
		}
		resultQuery := query(executor, config, domain, filterKeyword, apiKey)
		result = append(result, resultQuery...)
	}

	return
}

// keylessAPIs 无需 API Key 的搜索引擎（crt.sh 公共证书日志）。
// 这类 API 的 apiKey 为空是常态：空 key 直接放行执行，
// 不走「没有可用的key」快速失败路径（否则 MCP 扫描恒返回 0 结果）。
var keylessAPIs = map[string]bool{
	"crtsh": true,
}

// query 查询一个domain
func query(executor Executor, config execute.OnlineAPIConfig, domain string, filterKeyword map[string]struct{}, apiKey APIKey) (result []OnlineSearchResult) {
	queryString := executor.GetQueryString(domain, config, filterKeyword)
	pageResult, sizeTotal, err := retriedQuery(executor, config, queryString, 1, config.SearchPageSize, &apiKey)
	if err != nil {
		logging.RuntimeLog.Error(err)
		logging.CLILog.Error(err)
		return
	}
	if config.SearchLimitCount > 0 && sizeTotal > config.SearchLimitCount {
		msg := fmt.Sprintf("%s 搜索 %s 结果超过限制， total:%d, limited to:%d", apiKey.apiName, domain, sizeTotal, config.SearchLimitCount)
		logging.RuntimeLog.Warning(msg)
		sizeTotal = config.SearchLimitCount
	}
	result = append(result, pageResult...)
	pageTotalNum := sizeTotal / config.SearchPageSize
	if sizeTotal%config.SearchPageSize > 0 {
		pageTotalNum++
	}
	for i := 2; i <= pageTotalNum; i++ {
		pageResult, _, err = retriedQuery(executor, config, queryString, i, config.SearchPageSize, &apiKey)
		if err != nil {
			logging.RuntimeLog.Error(err)
			logging.CLILog.Error(err)
			return
		}
		result = append(result, pageResult...)
		time.Sleep(1 * time.Second)
	}
	return
}

// retriedQuery 执行一次查询，允许重试N次
func retriedQuery(executor Executor, config execute.OnlineAPIConfig, query string, pageIndex int, pageSize int, apiKey *APIKey) (pageResult []OnlineSearchResult, sizeTotal int, err error) {
	RETRIED := 3
	allKeys := strings.Split(apiKey.apiKey, ",")
	if len(allKeys) > 1 {
		RETRIED = 2 * len(allKeys)
	}
	if apiKey.apiKeyInUse == "" {
		if apiKey.apiKeyInUse = getOneAPIKey(apiKey); apiKey.apiKeyInUse == "" && !keylessAPIs[apiKey.apiName] {
			return nil, 0, errors.New(fmt.Sprintf("%s没有可用的key", apiKey.apiName))
		}
	}
	var retriedCount int
	for ; retriedCount < RETRIED; retriedCount++ {
		pageResult, sizeTotal, err = executor.Run(query, apiKey.apiKeyInUse, pageIndex, pageSize, config)
		if err == nil {
			return
		}
		msg := fmt.Sprintf("查询失败：%s -> key %s，error:%v", apiKey.apiName, desensitizationAPIKey(*apiKey), err)
		logging.RuntimeLog.Error(msg)
		logging.CLILog.Error(msg)

		if apiKey.apiKeyInUse = getOneAPIKey(apiKey); apiKey.apiKeyInUse == "" && !keylessAPIs[apiKey.apiName] {
			return nil, 0, errors.New(fmt.Sprintf("%s没有可用的key", apiKey.apiName))
		}
		time.Sleep(3 * time.Second)
	}
	if retriedCount >= RETRIED {
		return nil, 0, errors.New(fmt.Sprintf("%s 查询重试次数达到上限", apiKey.apiName))
	}
	return
}

// desensitizationAPIKey 脱敏APIKey，格式为xxxx****xxxx或者xxxx****
func desensitizationAPIKey(key APIKey) string {
	l := len(key.apiKeyInUse)
	if l == 0 {
		return ""
	}
	if l >= 4 {
		keyPre := key.apiKeyInUse[:4]
		if l >= 8 {
			var keyMid, keyEnd string
			if l >= 12 {
				keyMid = "****"
				keyEnd = key.apiKeyInUse[l-4:]
			} else {
				keyMid = ""
				keyEnd = "****"
			}
			return fmt.Sprintf("%s%s%s", keyPre, keyMid, keyEnd)
		}
		return fmt.Sprintf("%s****", keyPre)
	}
	return "****"
}

// getOneAPIKey 选择一个查询的key
func getOneAPIKey(apiKey *APIKey) string {
	allKeys := strings.Split(apiKey.apiKey, ",")

	if len(allKeys) == 0 {
		return ""
	}
	if len(allKeys) == 1 {
		return allKeys[0]
	}
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		n := r.Intn(len(allKeys))
		if allKeys[n] != apiKey.apiKeyInUse {
			return allKeys[n]
		}
	}
}

// loadFilterKeyword 从文件中加载需要过滤的标题关键词
func loadFilterKeyword() (filterKeyword map[string]struct{}) {
	filterKeyword = make(map[string]struct{})
	return
}
