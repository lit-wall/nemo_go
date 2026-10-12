package onlineapi

import (
	"errors"
	"strings"
	"testing"

	"github.com/hanc00l/nemo_go/v3/pkg/core"
	"github.com/hanc00l/nemo_go/v3/pkg/task/execute"
)

func getOnlineAPITaskInfo() execute.ExecutorTaskInfo {
	executorConfig := execute.ExecutorConfig{
		OnlineAPI: map[string]execute.OnlineAPIConfig{
			"fofa":   {},
			"hunter": {},
			"quake":  {},
		},
	}
	taskInfo := execute.ExecutorTaskInfo{
		MainTaskInfo: execute.MainTaskInfo{
			WorkspaceId:    "test",
			TargetMap:      map[string]string{},
			OrgId:          "test1",
			MainTaskId:     "onlineapi_test",
			ExecutorConfig: executorConfig,
		},
	}
	taskInfo.MainTaskInfo.ExecutorConfig = executorConfig

	return taskInfo
}

func TestResult_ParseResult(t *testing.T) {
	taskInfo := getOnlineAPITaskInfo()
	taskInfo.Executor = "fofa"
	result := Do(taskInfo)
	docs := ParseResult(taskInfo, result)
	for _, doc := range docs {
		t.Log(doc)
	}
}

// fakeExecutor 测试用假执行器：Run 立即返回固定结果，failNext 控制前N次失败（测试重试路径）。
type fakeExecutor struct {
	runCount int
	failNext int
}

func (f *fakeExecutor) GetRequiredResources() (re []core.RequiredResource) { return }

func (f *fakeExecutor) GetSyntaxMap() (syntax map[SyntaxType]string) {
	return map[SyntaxType]string{}
}

func (f *fakeExecutor) MakeSearchSyntax(syntax map[SyntaxType]string, condition SyntaxType, checkMod SyntaxType, value string) string {
	return value
}

func (f *fakeExecutor) GetQueryString(domain string, config execute.OnlineAPIConfig, filterKeyword map[string]struct{}) string {
	return domain
}

func (f *fakeExecutor) Run(query string, apiKey string, pageIndex int, pageSize int, config execute.OnlineAPIConfig) ([]OnlineSearchResult, int, error) {
	f.runCount++
	if f.failNext > 0 {
		f.failNext--
		return nil, 0, errors.New("模拟查询失败")
	}
	return []OnlineSearchResult{{Domain: "example.com"}}, 1, nil
}

// TestRetriedQuery_KeylessCRTSh 回归：crtsh 免key，空 apiKey 是常态，必须放行执行，
// 不能走「没有可用的key」快速失败（此前导致 MCP 扫描恒返回 0 结果）。
func TestRetriedQuery_KeylessCRTSh(t *testing.T) {
	fake := &fakeExecutor{}
	key := APIKey{apiName: "crtsh", apiKey: ""}
	pageResult, sizeTotal, err := retriedQuery(fake, execute.OnlineAPIConfig{SearchLimitCount: 10}, "blog", 1, 100, &key)
	if err != nil {
		t.Fatalf("crtsh 空key应放行执行，实际失败: %v", err)
	}
	if fake.runCount != 1 {
		t.Fatalf("期望 Run 执行1次，实际 %d 次", fake.runCount)
	}
	if sizeTotal != 1 || len(pageResult) != 1 {
		t.Fatalf("期望1条结果，实际 sizeTotal=%d len=%d", sizeTotal, len(pageResult))
	}
}

// TestRetriedQuery_KeyedAPIsStillFailFast key型API（fofa/hunter/quake）空key行为不变：执行前快速报错。
func TestRetriedQuery_KeyedAPIsStillFailFast(t *testing.T) {
	for _, name := range []string{"fofa", "hunter", "quake"} {
		fake := &fakeExecutor{}
		key := APIKey{apiName: name, apiKey: ""}
		_, _, err := retriedQuery(fake, execute.OnlineAPIConfig{}, "example.com", 1, 100, &key)
		if err == nil {
			t.Fatalf("%s 空key应报「没有可用的key」，实际放行了", name)
		}
		if !strings.Contains(err.Error(), "没有可用的key") {
			t.Fatalf("%s 期望「没有可用的key」错误，实际: %v", name, err)
		}
		if fake.runCount != 0 {
			t.Fatalf("%s 空key应执行前失败，实际执行了 %d 次", name, fake.runCount)
		}
	}
}

// TestRetriedQuery_KeylessRetry 免key API 首次失败后重试成功，且不经过「没有可用的key」分支。
func TestRetriedQuery_KeylessRetry(t *testing.T) {
	fake := &fakeExecutor{failNext: 1}
	key := APIKey{apiName: "crtsh", apiKey: ""}
	_, _, err := retriedQuery(fake, execute.OnlineAPIConfig{}, "blog", 1, 100, &key)
	if err != nil {
		t.Fatalf("重试后应成功，实际: %v", err)
	}
	if fake.runCount != 2 {
		t.Fatalf("期望执行2次（首次失败+重试成功），实际 %d 次", fake.runCount)
	}
}
