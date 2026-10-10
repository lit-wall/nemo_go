package core

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hanc00l/nemo_go/v3/pkg/cert"
	"github.com/hanc00l/nemo_go/v3/pkg/conf"
	"github.com/hanc00l/nemo_go/v3/pkg/logging"
	"github.com/hanc00l/nemo_go/v3/pkg/utils"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"golang.org/x/net/context"
)

const (
	AuthCtxKey      = "auth"
	AuthTokenName   = "X-Auth-Token"
	AuthWorkspaceId = "X-WorkspaceId"
)

type AuthInfo struct {
	Success     bool   `json:"success"`
	Token       string `json:"token"`
	WorkspaceId string `json:"workspaceId"`
}

type AssetListData struct {
	Authority  string   `json:"authority"`
	Host       string   `json:"host"`
	Domain     string   `json:"domain,,omitempty"`
	IP         []string `json:"ip"`
	Port       string   `json:"port"`
	Status     string   `json:"status,omitempty"`
	Location   []string `json:"location"`
	Service    string   `json:"service,omitempty"`
	Title      string   `json:"title,omitempty"`
	Header     string   `json:"header,omitempty"`
	Cert       string   `json:"cert,omitempty"`
	Banner     string   `json:"banner,omitempty"`
	App        []string `json:"app,omitempty"`
	IconHash   string   `json:"icon_hash,omitempty"`
	IsCDN      bool     `json:"cdn,omitempty"`
	IsHoneypot bool     `json:"honeypot,omitempty"`
	Vul        []string `json:"vul,omitempty"` // 漏洞列表
	Icp        string   `json:"icp,omitempty"` // 备案信息
	IcpCompany string   `json:"icp_company,omitempty"`
	Whois      string   `json:"whois,omitempty"`
	UpdateTime string   `json:"update_time"`
}

func NewMCPServer() *server.MCPServer {
	return server.NewMCPServer(
		"NemoV3 MCP Server 🚀",
		"1.0.0",
		server.WithToolCapabilities(true),
	)
}

// StartMCPServer 启动MCP服务
func StartMCPServer(s *server.MCPServer, serverAddr string) error {
	// 2. 创建SSE服务器
	sseServer := server.NewSSEServer(s,
		server.WithHTTPContextFunc(authFromRequest),
	)
	// 3.启动SSE服务器
	if err := sseServer.Start(serverAddr); err != nil {
		logging.CLILog.Error(err)
		return err
	}
	return nil
}

// StartMCPServerWithTLS 启动MCP服务，使用TLS加密传输
func StartMCPServerWithTLS(s *server.MCPServer, serverAddr string, certFile string, keyFile string) error {
	// 2. 加载TLS证书
	certPathFile := filepath.Join(conf.GetRootPath(), certFile)
	keyPathFile := filepath.Join(conf.GetRootPath(), keyFile)
	if !utils.CheckFileExist(certPathFile) || !utils.CheckFileExist(keyPathFile) {
		if err := cert.GenerateSelfSignedCert(certPathFile, keyPathFile); err != nil {
			logging.CLILog.Error(err)
			return err
		}
		logging.CLILog.Info("generate selfsigned cert...")
	}
	certConfig, err := tls.LoadX509KeyPair(certPathFile, keyPathFile)
	if err != nil {
		logging.CLILog.Errorf("加载证书失败: %v", err)
		return err
	} else {
		logging.CLILog.Info(certConfig)
	}
	// 3. 配置TLS
	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{certConfig},
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
	}
	// 4. 创建SSE服务器
	sseServer := server.NewSSEServer(s,
		server.WithHTTPContextFunc(authFromRequest),
	)
	// 5. 创建HTTP服务器
	httpServer := &http.Server{
		Addr:      serverAddr,
		TLSConfig: tlsConfig,
		Handler:   sseServer,
	}
	// 6. 启动 HTTPS 服务器
	if err = httpServer.ListenAndServeTLS("", ""); err != nil {
		return err
	}
	return nil
}

func authFromRequest(ctx context.Context, r *http.Request) context.Context {
	// 从header中获取token和workspaceId
	token := r.Header.Get(AuthTokenName)
	workspaceId := r.Header.Get(AuthWorkspaceId)
	if len(token) == 0 || len(workspaceId) == 0 {
		return context.WithValue(ctx, AuthCtxKey, AuthInfo{false, "", ""})
	}
	// 验证token
	for _, authToken := range conf.GlobalServerConfig().MCPServer.AuthToken {
		if token == authToken.Token {
			// 验证workspaceId
			for _, wid := range authToken.WorkspaceId {
				if workspaceId == wid {
					return context.WithValue(ctx, AuthCtxKey, AuthInfo{true, token, workspaceId})
				}
			}
			return context.WithValue(ctx, AuthCtxKey, AuthInfo{false, "", ""})
		}
	}

	return context.WithValue(ctx, AuthCtxKey, AuthInfo{false, "", ""})
}

func CheckAuth(ctx context.Context) (bool, string) {
	authInfo := ctx.Value(AuthCtxKey).(AuthInfo)
	return authInfo.Success, authInfo.WorkspaceId
}

func GetRequiredArgumentInt(request *mcp.CallToolRequest, name string) (int, error) {
	v, ok := request.Params.Arguments[name]
	if !ok {
		return 0, errors.New(fmt.Sprintf("缺少参数：%s", name))
	}
	return strconv.Atoi(v.(string))
}

func GetRequiredArgumentString(request *mcp.CallToolRequest, name string) (string, error) {
	v, ok := request.Params.Arguments[name]
	if !ok {
		return "", errors.New(fmt.Sprintf("缺少参数：%s", name))
	}
	return v.(string), nil
}

func GetRequiredArgumentBool(request *mcp.CallToolRequest, name string) (bool, error) {
	v, ok := request.Params.Arguments[name]
	if !ok {
		return false, errors.New(fmt.Sprintf("缺少参数：%s", name))
	}
	return v.(bool), nil
}

func GetDefaultArgumentInt(request *mcp.CallToolRequest, name string, defaultInt int) int {
	v, ok := request.Params.Arguments[name]
	if !ok {
		return defaultInt
	}
	switch t := v.(type) {
	case string:
		// 魔改：JSON-RPC 数字参数可能解码为 float64，原 v.(string)/v.(int) 硬断言会 panic
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return defaultInt
}

func GetDefaultArgumentString(request *mcp.CallToolRequest, name string, defaultString string) string {
	v, ok := request.Params.Arguments[name]
	if !ok {
		return defaultString
	}
	return v.(string)
}

func GetDefaultArgumentBool(request *mcp.CallToolRequest, name string, defaultBool bool) bool {
	v, ok := request.Params.Arguments[name]
	if !ok {
		return defaultBool
	}
	return v.(bool)
}

func GetWorkspaceIdFromContext(ctx context.Context) (string, error) {
	authInfo := ctx.Value(AuthCtxKey)
	if authInfo == nil {
		return "", errors.New(fmt.Sprintf("不正确的请求或验证失败，请检查请求头"))
	}
	workspaceId := authInfo.(AuthInfo).WorkspaceId
	if len(workspaceId) == 0 {
		return "", errors.New(fmt.Sprintf("请指定工作空间ID"))
	}
	return workspaceId, nil

}

// FormatDateTime 日期统一格式化
func FormatDateTime(dt time.Time) string {
	return dt.In(conf.LocalTimeLocation).Format("2006-01-02 15:04:05")
}

func SuccessResponse(msg string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText(msg), nil
}
func FailResponse(msg string) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(fmt.Sprintf("错误：%s", msg)), fmt.Errorf("错误：%s", msg)
}

func FailResponseError(msg string, err error) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(fmt.Sprintf("错误：%s", msg)), err
}
