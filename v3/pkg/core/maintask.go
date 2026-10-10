package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/RichardKnop/machinery/v2/tasks"
	"github.com/google/uuid"
	"github.com/hanc00l/nemo_go/v3/pkg/db"
	"github.com/hanc00l/nemo_go/v3/pkg/logging"
	"github.com/hanc00l/nemo_go/v3/pkg/task/execute"
	"github.com/hanc00l/nemo_go/v3/pkg/utils"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const (
	CREATED string = "CREATED" //任务创建，但还没有开始执行
	STARTED string = "STARTED" //任务在执行中
	SUCCESS string = "SUCCESS" //任务执行完成，结果为SUCCESS
	FAILURE string = "FAILURE" //任务执行完成，结果为FAILURE

	TopicActive     = "active"
	TopicFinger     = "finger"
	TopicPassive    = "passive"
	TopicPocscan    = "pocscan"
	TopicCustom     = "custom"
	TopicStandalone = "standalone"

	TopicMQPrefix = "nemo_mq"
)

var (
	globalMainTaskLock       = "main_task_lock"
	globalMainTaskUpdateTime = "main_task_update_time"
	globalStandaloneTaskLock = "standalone_task_lock"
)

// StartMainTaskDamon MainTask任务的后台监控
func StartMainTaskDamon() {
	const (
		BaseInterval = 10 * time.Second // 基础固定间隔
		MaxJitter    = 10 * time.Second // 最大随机增加量
	)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	redisClient, err := GetRedisClient()
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return
	}
	defer func(client *redis.Client) {
		_ = CloseRedisClient(client)
	}(redisClient)

	for {
		// 随机睡眠
		jitter := time.Duration(r.Int63n(int64(MaxJitter) + 1))
		sleepTime := BaseInterval + jitter
		time.Sleep(sleepTime)
		// 尝试获取锁
		lock := NewRedisLock(globalMainTaskLock, BaseInterval, redisClient)
		acquired, err := lock.TryLock()
		if err != nil {
			logging.RuntimeLog.Error("获取分布式锁失败:", err.Error())
			continue
		}
		if !acquired {
			// 未获取到锁
			logging.RuntimeLog.Warn("maintask未能获得分布式锁, sleep...")
			continue
		}
		mainTaskUpdateTime, err := getTimeFromRedis(redisClient, globalMainTaskUpdateTime)
		// 如果有多个service实例，为了避免重复处理，设置一个时间间隔，防止多个实例同时处理任务
		if errors.Is(err, redis.Nil) || time.Now().Sub(mainTaskUpdateTime).Seconds() >= BaseInterval.Seconds() {
			// 处理已开始的任务
			processStartedTask()
			// 处理新建的任务
			processCreatedTask()
			// 检查worker状态
			checkWorkerStatus()
			// 存储更新时间
			err = storeTimeToRedis(redisClient, globalMainTaskUpdateTime, time.Now())
			if err != nil {
				logging.RuntimeLog.Error("更新maintask的更新时间失败:", err.Error())
			}
			// 释放锁
			if unlockErr := lock.Unlock(); unlockErr != nil {
				logging.RuntimeLog.Error("释放maintask锁失败:", unlockErr.Error())
			}
		} else {
			if err != nil {
				logging.RuntimeLog.Error("获取maintask的更新时间失败:", err.Error())
			}
		}
	}
}

func processStartedTask() {
	client, err := db.GetClient()
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return
	}
	defer db.CloseClient(client)

	mainTaskDocs, err := db.NewMainTask(client).Find(bson.M{db.Status: STARTED, db.Cron: false}, 0, 0)
	if err != nil {
		logging.RuntimeLog.Error("获取maintask失败:", err.Error())
		return
	}
	for _, task := range mainTaskDocs {
		var status, progress, result string
		var progressRate float64
		// 检查子任务的执行情况：
		createdTask, startedTask, totalTask := checkExecutorTask(task.TaskId)
		progress = fmt.Sprintf("%d/%d/%d", startedTask, createdTask, totalTask)
		if totalTask > 0 && createdTask == 0 && startedTask == 0 {
			// 任务执行完成
			status = SUCCESS
			progressRate = 1.0
			//全部任务完成，将任务的结果同步到全局资产库
			result = SyncTaskAsset(task.WorkspaceId, task.TaskId)
			// 任务执行完成，发送消息
			workspace := db.NewWorkspace(client)
			wDoc, _ := workspace.GetByWorkspaceId(task.WorkspaceId)
			if len(wDoc.NotifyId) > 0 {
				_ = Notify(wDoc.NotifyId, NotifyData{
					TaskName: task.TaskName,
					Target:   task.Target,
					Runtime:  fmt.Sprintf("%s", time.Now().Sub(*task.StartTime)),
					Result:   result,
				})
			}
			// 生成报告
			if len(task.ReportLLMApi) > 0 {
				go GenerateReport(task.WorkspaceId, task.Id.Hex(), task.TaskId, task.ReportLLMApi)
			}
		} else {
			// 任务执行中或未执行
			status = task.Status
			progressRate = computeMainTaskProgressRate(task.TaskId, task.Args)
		}
		// 更新任务状态
		if status != task.Status || progress != task.Progress || progressRate != task.ProgressRate {
			if updateMainTask(task.Id.Hex(), status, progress, progressRate, result) == false {
				logging.RuntimeLog.Errorf("更新maintask：%s 任务状态失败:", task.TaskId)
				continue
			}
		}
	}

	return
}

func processCreatedTask() {
	client, err := db.GetClient()
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return
	}
	defer db.CloseClient(client)
	mainTask := db.NewMainTask(client)
	mainTaskDocs, err := mainTask.Find(bson.M{db.Status: CREATED, db.Cron: false}, 0, 0)
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return
	}
	for _, doc := range mainTaskDocs {
		var exeConfig execute.ExecutorConfig
		err = json.Unmarshal([]byte(doc.Args), &exeConfig)
		if err != nil {
			logging.RuntimeLog.Error(err.Error())
			return
		}
		mainTaskInfo := execute.MainTaskInfo{
			TargetMap:       utils.UnmarshalTargetMap(doc.Target),
			ExcludeTarget:   doc.ExcludeTarget,
			ExecutorConfig:  exeConfig,
			OrgId:           doc.OrgId,
			WorkspaceId:     doc.WorkspaceId,
			MainTaskId:      doc.TaskId,
			IsProxy:         doc.IsProxy,
			TargetSliceType: doc.TargetSliceType,
			TargetSliceNum:  doc.TargetSliceNum,
		}

		if err = processExecutorTask(mainTaskInfo); err != nil {
			return
		}
		// 任务启动，更新状态
		var isSuccess bool
		isSuccess, err = mainTask.Update(doc.Id.Hex(), bson.M{db.Status: STARTED, db.StartTime: time.Now()})
		if err != nil {
			logging.RuntimeLog.Error("更新maintask状态失败:", err.Error())
			continue
		}
		if !isSuccess {
			logging.RuntimeLog.Errorf("更新maintask失败， docId:%s, err:%v", doc.Id, err)
			continue
		}
	}

	return
}

func processExecutorTask(mainTaskInfo execute.MainTaskInfo) (err error) {
	f := func(executor string, mainTaskInfo execute.MainTaskInfo) (err error) {
		executorTaskInfo := execute.ExecutorTaskInfo{
			MainTaskInfo: mainTaskInfo,
			Executor:     executor,
			TaskId:       uuid.New().String(),
		}
		err = NewExecutorTask(executorTaskInfo)
		if err != nil {
			logging.RuntimeLog.Errorf("创建executor任务失败,mainTaskInfo:%v, err:%v", executorTaskInfo, err)
			return
		}
		return nil
	}
	handleFingerprintAndPocscan := func(typeKey string, mainTaskInfo execute.MainTaskInfo) (err error) {
		ts := NewTaskSlice(mainTaskInfo.TargetMap[typeKey], mainTaskInfo.TargetSliceType, mainTaskInfo.TargetSliceNum)
		targets := ts.SplitTargets()
		for _, target := range targets {
			mti := mainTaskInfo
			mti.TargetMap = make(map[string]string)
			mti.TargetMap[typeKey] = target
			//　如果同时有fingerprint和pocscan，则先执行fingerprint，pocscan在fingerprint执行完成后由任务启动下一个流程执行
			if len(mainTaskInfo.ExecutorConfig.FingerPrint) > 0 {
				// fingerprint不区分executor，由执行行时根据任务配置决定
				if err = f(execute.FingerPrint, mti); err != nil {
					return err
				}
			} else {
				for executor := range mainTaskInfo.ExecutorConfig.PocScan {
					if err = f(executor, mti); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	// 单独的standalone任务
	if len(mainTaskInfo.ExecutorConfig.Standalone) > 0 {
		ts := NewTaskSlice(mainTaskInfo.TargetMap[execute.TargetIp], mainTaskInfo.TargetSliceType, mainTaskInfo.TargetSliceNum)
		targets := ts.SplitTargets()
		for _, target := range targets {
			mti := mainTaskInfo
			mti.TargetMap = make(map[string]string)
			mti.TargetMap[execute.TargetIp] = target
			for executor := range mainTaskInfo.ExecutorConfig.Standalone {
				if err = f(executor, mti); err != nil {
					return err
				}
			}
		}
	}
	// ip任务：portscan、onlineapi
	if mainTaskInfo.TargetMap[execute.TargetIp] != "" {
		ts := NewTaskSlice(mainTaskInfo.TargetMap[execute.TargetIp], mainTaskInfo.TargetSliceType, mainTaskInfo.TargetSliceNum)
		targets := ts.SplitTargets()
		for _, target := range targets {
			mti := mainTaskInfo
			mti.TargetMap = make(map[string]string)
			mti.TargetMap[execute.TargetIp] = target
			for executor := range mainTaskInfo.ExecutorConfig.PortScan {
				if err = f(executor, mti); err != nil {
					return err
				}
			}
			for executor := range mainTaskInfo.ExecutorConfig.OnlineAPI {
				if err = f(executor, mti); err != nil {
					return err
				}
			}
		}
	}
	// 主域名任务：domainscan、onlineapi、icpPlus2
	if mainTaskInfo.TargetMap[execute.TargetRootDomain] != "" {
		ts := NewTaskSlice(mainTaskInfo.TargetMap[execute.TargetRootDomain], mainTaskInfo.TargetSliceType, mainTaskInfo.TargetSliceNum)
		targets := ts.SplitTargets()
		for _, target := range targets {
			mti := mainTaskInfo
			mti.TargetMap = make(map[string]string)
			mti.TargetMap[execute.TargetRootDomain] = target
			for executor := range mainTaskInfo.ExecutorConfig.DomainScan {
				if err = f(executor, mti); err != nil {
					return err
				}
			}
			for executor := range mainTaskInfo.ExecutorConfig.OnlineAPI {
				if err = f(executor, mti); err != nil {
					return err
				}
			}
			for executor := range mainTaskInfo.ExecutorConfig.ICP {
				if executor == "icpPlus2" {
					if err = f(executor, mti); err != nil {
						return err
					}
				}
			}
		}
	}
	// 子域名任务：domainscan
	if mainTaskInfo.TargetMap[execute.TargetSubDomain] != "" {
		ts := NewTaskSlice(mainTaskInfo.TargetMap[execute.TargetSubDomain], mainTaskInfo.TargetSliceType, mainTaskInfo.TargetSliceNum)
		targets := ts.SplitTargets()
		for _, target := range targets {
			mti := mainTaskInfo
			mti.TargetMap = make(map[string]string)
			mti.TargetMap[execute.TargetSubDomain] = target
			for executor := range mainTaskInfo.ExecutorConfig.DomainScan {
				if err = f(executor, mti); err != nil {
					return err
				}
			}
		}
	}

	// endpoint任务：fingerprint、pocscan
	if mainTaskInfo.TargetMap[execute.TargetEndpoint] != "" {
		if err = handleFingerprintAndPocscan(execute.TargetEndpoint, mainTaskInfo); err != nil {
			return err
		}
	}
	// url任务：fingerprint、pocscan
	if mainTaskInfo.TargetMap[execute.TargetUrl] != "" {
		if err = handleFingerprintAndPocscan(execute.TargetUrl, mainTaskInfo); err != nil {
			return err
		}
	}
	// unit任务：icpPlus、onlineapi（零种子dork模式使用unit键）
	if mainTaskInfo.TargetMap[execute.TargetUnit] != "" {
		ts := NewTaskSlice(mainTaskInfo.TargetMap[execute.TargetUnit], mainTaskInfo.TargetSliceType, mainTaskInfo.TargetSliceNum)
		targets := ts.SplitTargets()
		for _, target := range targets {
			mti := mainTaskInfo
			mti.TargetMap[execute.TargetUnit] = target
			for executor := range mainTaskInfo.ExecutorConfig.ICP {
				if executor == "icpPlus" {
					if err = f(executor, mti); err != nil {
						return err
					}
				}
			}
			for executor := range mainTaskInfo.ExecutorConfig.OnlineAPI {
				if err = f(executor, mti); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func computeTaskCategoryRate(exeConfig execute.ExecutorConfig) (categoryRate map[string]float64) {
	categoryRate = make(map[string]float64)
	taskCategoryTotal := 0
	if len(exeConfig.LLMAPI) > 0 {
		taskCategoryTotal++
	}
	if len(exeConfig.ICP) > 0 {
		taskCategoryTotal++
	}
	if len(exeConfig.PortScan) > 0 {
		taskCategoryTotal++
	}
	if len(exeConfig.DomainScan) > 0 {
		taskCategoryTotal++
	}
	if len(exeConfig.OnlineAPI) > 0 {
		taskCategoryTotal++
	}
	if len(exeConfig.FingerPrint) > 0 {
		taskCategoryTotal++
	}
	if len(exeConfig.PocScan) > 0 {
		taskCategoryTotal++
	}
	taskCategoryRate := 1.0 / float64(taskCategoryTotal)
	if len(exeConfig.FingerPrint) > 0 {
		categoryRate[execute.FingerPrint] = taskCategoryRate
	} else {
		categoryRate[execute.FingerPrint] = 0.0
	}
	if len(exeConfig.PocScan) > 0 {
		categoryRate[execute.PocScan] = taskCategoryRate
	} else {
		categoryRate[execute.PocScan] = 0.0
	}
	if len(exeConfig.PortScan) > 0 {
		categoryRate[execute.PortScan] = taskCategoryRate
	} else {
		categoryRate[execute.PortScan] = 0.0
	}
	if len(exeConfig.DomainScan) > 0 {
		categoryRate[execute.DomainScan] = taskCategoryRate
	} else {
		categoryRate[execute.DomainScan] = 0.0
	}
	if len(exeConfig.OnlineAPI) > 0 {
		categoryRate[execute.OnlineAPI] = taskCategoryRate
	} else {
		categoryRate[execute.OnlineAPI] = 0.0
	}
	if len(exeConfig.LLMAPI) > 0 {
		categoryRate[execute.LLMAPI] = taskCategoryRate
	} else {
		categoryRate[execute.LLMAPI] = 0
	}
	if len(exeConfig.ICP) > 0 {
		categoryRate[execute.ICP] = taskCategoryRate
	} else {
		categoryRate[execute.ICP] = 0
	}
	if len(exeConfig.Standalone) > 0 {
		categoryRate[execute.Standalone] = 1
	} else {
		categoryRate[execute.Standalone] = 0
	}

	return
}

func computeMainTaskProgressRate(mainTaskId string, args string) (result float64) {
	// 聚合每个executor的任务状态
	client, err := db.GetClient()
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return
	}
	defer db.CloseClient(client)
	executeTask := db.NewExecutorTask(client)
	executorTaskResult, err := executeTask.AggregateMainTaskProgress(mainTaskId)
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return
	}
	if executorTaskResult == nil {
		return
	}
	// 根据executor名称，统计不同分类的任务的不同状态的数量
	total := make(map[string]map[string]int)
	total[execute.PortScan] = make(map[string]int)
	total[execute.DomainScan] = make(map[string]int)
	total[execute.OnlineAPI] = make(map[string]int)
	total[execute.FingerPrint] = make(map[string]int)
	total[execute.PocScan] = make(map[string]int)
	total[execute.Standalone] = make(map[string]int)
	total[execute.LLMAPI] = make(map[string]int)
	total[execute.ICP] = make(map[string]int)
	for _, r := range executorTaskResult {
		for _, s := range r.Statuses {
			switch r.Executor {
			case "masscan", "nmap", "gogo":
				total[execute.PortScan][s.Status] += s.Count
			case "subfinder", "massdns":
				total[execute.DomainScan][s.Status] += s.Count
			case "fofa", "hunter", "quake":
				total[execute.OnlineAPI][s.Status] += s.Count
			case "fingerprint":
				total[execute.FingerPrint][s.Status] += s.Count
			case "nuclei", "zombie":
				total[execute.PocScan][s.Status] += s.Count
			case "qwen", "kimi", "deepseek":
				total[execute.LLMAPI][s.Status] += s.Count
			case "icp", "icpPlus", "icpPlus2":
				total[execute.ICP][s.Status] += s.Count
			case "standalone":
				total[execute.Standalone][s.Status] += s.Count
			}
		}
	}
	// 根据分类比例，计算每个分类的任务比例
	var exeConfig execute.ExecutorConfig
	err = json.Unmarshal([]byte(args), &exeConfig)
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return
	}
	taskCategoryRateScore := computeTaskCategoryRate(exeConfig)
	// 计算总进度
	for category, status := range total {
		var created, started, finished int
		if v, ok := status[CREATED]; ok {
			created = v
		}
		if v, ok := status[STARTED]; ok {
			started = v
		}
		if v, ok := status[SUCCESS]; ok {
			finished += v
		}
		if v, ok := status[FAILURE]; ok {
			finished += v
		}
		if created+started+finished == 0 {
			continue
		}
		result += (float64)(finished) / (float64)(created+started+finished) * taskCategoryRateScore[category]
	}

	return
}

func NewExecutorTask(executorTaskInfo execute.ExecutorTaskInfo) (err error) {
	// 检查目标中是否有黑名单
	blackTarget, newTargetMap := checkTargetMapForBlacklist(executorTaskInfo.TargetMap, executorTaskInfo.WorkspaceId)
	// 黑名单处理
	if len(blackTarget) > 0 {
		logging.RuntimeLog.Warnf("匹配到黑名单记录: %s, skip", strings.Join(blackTarget, ","))
	}
	// 正常目标处理
	executorTaskInfo.TargetMap = newTargetMap
	//　standalone任务不送入消息队列，只写入数据库；其他任务送入消息队列
	if executorTaskInfo.Executor != "standalone" {
		if err = sendExecutorTaskToMq(executorTaskInfo); err != nil {
			return
		}
	}
	if err = addExecutorTaskToDb(executorTaskInfo); err != nil {
		return
	}
	return nil
}

func checkTargetMapForBlacklist(targetMap map[string]string, workspaceId string) (blacklist []string, targetMapNew map[string]string) {
	targetMapNew = make(map[string]string)
	blc := NewBlacklist()
	blc.LoadBlacklist(workspaceId)
	for k, v := range targetMap {
		if len(v) == 0 {
			continue
		}
		var targetNew []string
		// 检查目标中是否有黑名单
		for _, t := range strings.Split(v, ",") {
			targetStripped := strings.TrimSpace(t)
			if targetStripped == "" {
				continue
			}
			hostPort := strings.Split(targetStripped, ":")
			if len(hostPort) == 0 {
				continue
			}
			host := hostPort[0]
			if blc.IsHostBlocked(host) {
				blacklist = append(blacklist, targetStripped)
			} else {
				targetNew = append(targetNew, targetStripped)
			}
		}
		if len(targetNew) > 0 {
			targetMapNew[k] = strings.Join(targetNew, ",")
		}
	}
	return
}

func sendExecutorTaskToMq(executorTaskInfo execute.ExecutorTaskInfo) (err error) {
	topicName := GetTopicByTaskName(executorTaskInfo.Executor, executorTaskInfo.WorkspaceId)
	if topicName == "" {
		msg := fmt.Sprintf("任务没有配置topic:%s", executorTaskInfo.Executor)
		logging.RuntimeLog.Error(msg)
		return errors.New(msg)
	}
	configJSON, err := json.Marshal(executorTaskInfo)
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return err
	}
	server := GetServerTaskMQServer(topicName)
	// 延迟5秒后执行：如果不延迟，有可能任务在完成数据库之前执行，从而导致task not exist错误
	eta := time.Now().Add(time.Second * 5)
	workerTask := tasks.Signature{
		Name: executorTaskInfo.Executor,
		UUID: executorTaskInfo.TaskId,
		ETA:  &eta,
		Args: []tasks.Arg{
			{Name: "configJSON", Type: "string", Value: string(configJSON)},
		},
		//RoutingKey：分发到不同功能的worker队列
		RoutingKey: GetRoutingKeyByTopic(topicName),
	}
	_, err = server.SendTask(&workerTask)
	if err != nil {
		logging.RuntimeLog.Error(err)
		return err
	}

	return nil
}

func addExecutorTaskToDb(executorTaskInfo execute.ExecutorTaskInfo) error {
	doc := db.ExecuteTaskDocument{
		WorkspaceId:   executorTaskInfo.WorkspaceId,
		TaskId:        executorTaskInfo.TaskId,
		MainTaskId:    executorTaskInfo.MainTaskId,
		PreTaskId:     executorTaskInfo.PreTaskId,
		Executor:      executorTaskInfo.Executor,
		Target:        utils.MarshalTargetMap(executorTaskInfo.TargetMap),
		ExcludeTarget: executorTaskInfo.ExcludeTarget,
		Status:        CREATED,
	}
	if argsData, err := json.Marshal(executorTaskInfo.ExecutorConfig); err != nil {
		logging.RuntimeLog.Error(err.Error())
		return err
	} else {
		doc.Args = string(argsData)
	}
	mongoClient, err := db.GetClient()
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return err
	}
	defer db.CloseClient(mongoClient)

	isSuccess, err := db.NewExecutorTask(mongoClient).Insert(doc)
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return err
	}
	if !isSuccess {
		logging.RuntimeLog.Errorf("生成子任务失败, doc:%v, err:%v", doc, err)
		return errors.New("生成子任务失败")
	}
	return nil
}

func checkExecutorTask(mainTaskId string) (createdTask, startedTask, totalTask int) {
	mongoClient, err := db.GetClient()
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return
	}
	defer db.CloseClient(mongoClient)

	runTasks, err := db.NewExecutorTask(mongoClient).Find(bson.M{db.MainTaskId: mainTaskId}, 0, 0)
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return
	}
	for _, t := range runTasks {
		if t.Status == CREATED {
			createdTask++
		} else if t.Status == STARTED {
			startedTask++
		}
	}
	totalTask = len(runTasks)
	return
}

func updateMainTask(id string, state string, progress string, progressRate float64, result string) bool {
	update := bson.M{}
	if state != "" {
		update[db.Status] = state
		if state == SUCCESS || state == FAILURE {
			update[db.EndTime] = time.Now()
		}
	}
	if progress != "" {
		update[db.Progress] = progress
	}
	if result != "" {
		update[db.Result] = result
	}
	if progressRate != 0 {
		update[db.ProgressRate] = progressRate
	}
	client, err := db.GetClient()
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return false
	}
	defer db.CloseClient(client)

	isSuccess, err := db.NewMainTask(client).Update(id, update)
	if err != nil {
		logging.RuntimeLog.Error(err.Error())
		return false
	}
	return isSuccess
}

func ParseStringTargetToTargetMap(targetStr string, isTldRootDomain bool) (targetMap map[string][]string) {
	targetMap = make(map[string][]string)
	targetMapSet := make(map[string]map[string]struct{})
	targetMapSet[execute.TargetIp] = make(map[string]struct{})
	targetMapSet[execute.TargetRootDomain] = make(map[string]struct{})
	targetMapSet[execute.TargetSubDomain] = make(map[string]struct{})
	targetMapSet[execute.TargetUrl] = make(map[string]struct{})
	targetMapSet[execute.TargetEndpoint] = make(map[string]struct{})
	targetMapSet[execute.TargetUnit] = make(map[string]struct{})
	tld := utils.NewTldExtract()

	for _, t := range strings.Split(targetStr, ",") {
		target := strings.TrimSpace(t)
		if target == "" {
			continue
		}
		if utils.CheckIPOrSubnet(target) {
			targetMapSet[execute.TargetIp][target] = struct{}{}
			continue
		}
		if utils.CheckDomain(target) {
			domainTldFromTarget := tld.ExtractFLD(target)
			if domainTldFromTarget == target {
				targetMapSet[execute.TargetRootDomain][target] = struct{}{}
			} else {
				targetMapSet[execute.TargetSubDomain][target] = struct{}{}
				if isTldRootDomain {
					targetMapSet[execute.TargetRootDomain][domainTldFromTarget] = struct{}{}
				}
			}
			continue
		}
		if CheckURL(target) {
			targetMapSet[execute.TargetUrl][target] = struct{}{}
			continue
		}
		if CheckEndpoint(target) {
			targetMapSet[execute.TargetEndpoint][target] = struct{}{}
			continue
		}
		if CheckCompanyName(target) {
			targetMapSet[execute.TargetUnit][target] = struct{}{}
			continue
		}
		logging.RuntimeLog.Warnf("目标格式错误: %s", target)
	}
	for k, v := range targetMapSet {
		if len(v) > 0 {
			targetMap[k] = utils.SetToSlice(v)
		}
	}
	return targetMap
}

// CheckURL 检查字符串是否为有效的URL格式（仅验证协议前缀）
// 参数：url - 要检查的字符串
// 返回值：如果是有效的URL格式返回true，否则返回false
func CheckURL(url string) bool {
	// 完整的常用URL协议列表
	urlPrefixes := []string{
		// 网络协议
		"http://", "https://",
		"ftp://", "ftps://",
		"ws://", "wss://", // WebSocket
		"rtsp://", "rtmp://", // 流媒体
		"udp://", "tcp://",

		// 文件相关
		"file://",
		"smb://", // Samba共享
		"nfs://", // 网络文件系统

		// 邮件相关
		"mailto:", "smtp://",
		"imap://", "pop3://",

		// 数据库
		"mysql://", "postgres://",
		"mongodb://", "redis://",

		// 消息队列
		"amqp://", "mqtt://",
		"kafka://",

		// 云存储
		"s3://", // AWS S3
		"gs://", // Google Cloud Storage
		"az://", // Azure Blob Storage

		// 特殊协议
		"telnet://", "ssh://",
		"git://", "svn://",
		"ldap://", "ldaps://",
		"irc://", "ircs://",

		// 移动应用
		"whatsapp://", "wechat://",
	}

	lowerURL := strings.ToLower(url)

	for _, prefix := range urlPrefixes {
		if strings.HasPrefix(lowerURL, prefix) {
			return true
		}
	}
	return false
}

// CheckCompanyName 检查输入字符串是否可能为公司或组织名称（宽松版本）
// 适用于准确性要求不高的场景，如初步筛选、表单验证等
func CheckCompanyName(name string) bool {
	name = strings.TrimSpace(name)

	// 基本长度检查
	if len(name) < 2 || len(name) > 100 {
		return false
	}

	// 不能全部是数字、符号或空格
	if isAllNonLetters(name) {
		return false
	}

	// 必须包含至少2个字母或汉字
	if !containsSufficientLetters(name) {
		return false
	}

	// 检查是否包含常见公司标识词（宽松匹配）
	return containsCompanyIndicators(name)
}

// 常见公司组织标识词（中英文）
var companyIndicators = []string{
	// 中文标识
	"公司", "有限", "股份", "集团", "控股", "实业", "科技", "技术",
	"信息", "网络", "软件", "智能", "咨询", "管理", "服务", "发展",
	"投资", "金融", "证券", "银行", "保险", "基金", "租赁", "经纪",
	"事务所", "协会", "学会", "基金会", "中心", "研究院", "研究所",
	"实验室", "俱乐部", "联盟", "商会", "促进会", "联合会", "委员会",
	"企业", "工厂", "商店", "超市", "商场", "酒店", "饭店", "餐饮",
	"贸易", "商贸", "商业", "商务", "电子", "电器", "机械", "设备",
	"制造", "工程", "建设", "建筑", "装饰", "设计", "广告", "传媒",
	"文化", "教育", "培训", "学校", "学院", "大学", "医院", "医疗",
	"医药", "健康", "环保", "能源", "电力", "化工", "材料", "物流",
	"运输", "快递", "航空", "旅游", "房地产", "物业", "农业",

	// 英文标识
	"inc", "ltd", "llc", "corp", "co", "gmbh", "ag", "group",
	"holdings", "limited", "incorporated", "corporation", "company",
	"international", "global", "enterprises", "ventures", "partners",
	"tech", "technology", "software", "network", "digital", "solution",
	"service", "financial", "investment", "capital", "management",
	"consulting", "development", "research", "laboratory", "institute",
}

// isAllNonLetters 检查是否全是非字母字符
func isAllNonLetters(s string) bool {
	for _, char := range s {
		if unicode.IsLetter(char) {
			return false
		}
	}
	return true
}

// containsSufficientLetters 检查是否包含足够的字母/汉字
func containsSufficientLetters(s string) bool {
	count := 0
	for _, char := range s {
		if unicode.IsLetter(char) {
			count++
			if count >= 2 {
				return true
			}
		}
	}
	return false
}

// containsCompanyIndicators 检查是否包含公司组织标识词
func containsCompanyIndicators(name string) bool {
	lowerName := strings.ToLower(name)

	for _, indicator := range companyIndicators {
		if strings.Contains(lowerName, indicator) {
			return true
		}
	}

	// 如果没有匹配到标识词，但格式看起来像公司名，也返回true
	return looksLikeCompanyName(name)
}

// looksLikeCompanyName 通过简单规则判断是否像公司名称
func looksLikeCompanyName(name string) bool {
	// 包含空格或点，且长度适中
	if strings.Contains(name, " ") || strings.Contains(name, ".") || strings.Contains(name, "·") {
		return len(name) >= 4 && len(name) <= 60
	}

	// 中文名称通常不包含空格但有一定长度
	if containsChinese(name) && len(name) >= 4 {
		return true
	}

	return false
}

// containsChinese 检查是否包含中文字符
func containsChinese(s string) bool {
	for _, char := range s {
		if unicode.In(char, unicode.Han) {
			return true
		}
	}
	return false
}

func CheckEndpoint(endpoint string) bool {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return false
	}

	// 检查host是否有效
	if host == "" {
		return false
	}

	// 检查port是否有效
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return false
	}

	return true
}
