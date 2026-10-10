# =============================================================================
# AutoShow 魔改版 Dockerfile（基于 hanc00l/nemo_go v3.2.1 原版修改）
# 原版要求：预先 build.sh 出 daemon_worker_linux_amd64 放进构建上下文，
#           且仓库内缺少 nuclei/fingerprintx/subfinder/massdns/gogo/zombie 二进制。
# 魔改点：
#   1. 多阶段构建：golang:1.23 阶段编译 server/worker/daemon_worker 三个核心
#      二进制（含本项目的机器令牌登录 / MCP authToken / fingers v1.2.1 适配）
#   2. runtime 阶段直接取官方 v3.2.1 release 包自带的 thirdparty/：六个工具
#      二进制 + dict/qqwry/ipv6wry/tldextract/geolite2/nmap 数据文件，全部是
#      上游与 nemo v3.2.1 配套测试过的版本（原"逐工具 GitHub release 下载"
#      方案因上游仓库 404 / release 无资产而废弃）
#   3. 重写 sources.list 启用 universe 组件（chrome 依赖）
# =============================================================================

# ---------- Stage 1: Go 编译（仅核心三件套；subfinder 等工具来自官方 release 包） ----------
FROM golang:1.23 AS gobuild
WORKDIR /src
# 先拷全量源码，再 tidy（同步 replace 后的 go.sum）——若把 tidy 放在 COPY . 之前，
# 宿主版的 go.mod/go.sum 会覆盖 tidy 结果，导致 replace 目标缺 go.sum 条目。
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod GOTOOLCHAIN=auto go mod tidy && GOTOOLCHAIN=auto go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=auto go build -ldflags "-s -w" -trimpath -o /out/server_linux_amd64 cmd/server/main.go \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=auto go build -ldflags "-s -w" -trimpath -o /out/worker_linux_amd64 cmd/worker/main.go \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=auto go build -ldflags "-s -w" -trimpath -o /out/daemon_worker_linux_amd64 cmd/daemon_worker/main.go

# ---------- Stage 2: Runtime ----------
FROM ubuntu:22.04

ENV LC_ALL=C.UTF-8 \
    TZ=Asia/Shanghai \
    DEBIAN_FRONTEND=noninteractive

# 魔改：重写 sources.list 启用 main/restricted/universe/multiverse 全组件
# （原版仅 main/restricted，chrome 的 libvulkan1/libx11-6 等依赖在 universe，缺组件导致构建失败）
RUN { for suite in jammy jammy-updates jammy-backports jammy-security; do \
        echo "deb http://mirrors.aliyun.com/ubuntu/ ${suite} main restricted universe multiverse"; \
      done; } > /etc/apt/sources.list \
    && ln -fs /usr/share/zoneinfo/${TZ} /etc/localtime \
    && echo "${TZ}" > /etc/timezone

# 先装 curl（下载官方 release 包 / chrome 用）
RUN apt update \
    && apt install -y --no-install-recommends curl ca-certificates \
    && apt clean

# 系统级扫描工具 + Chrome（指纹截图）；索引列表复用上一层，末尾清理
RUN curl -fSL -o /tmp/google-chrome-stable_current_amd64.deb \
       https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb \
    && apt install -y \
       tzdata vim net-tools iputils-ping unzip \
       nmap masscan /tmp/google-chrome-stable_current_amd64.deb --fix-missing \
    && rm -f /tmp/google-chrome-stable_current_amd64.deb \
    && apt clean && rm -rf /var/lib/apt/lists/*

# 官方 v3.2.1 release 包取 thirdparty/：
# nuclei/subfinder/gogo/massdns/fingerprintx/zombie/httpx 二进制 +
# dict/qqwry/ipv6wry/tldextract/geolite2/nmap 数据文件。
# 版本与 nemo v3.2.1 上游测试完全一致，worker 资源同步机制（CheckRequiredResource）
# 在 server 容器 /opt/nemo/thirdparty 内可直接命中。
# （只解出 thirdparty/，避免包内原版 conf/ 覆盖本项目魔改的 server.yml/worker.yml）
RUN curl -fSL -o /tmp/nemo-official.tar \
       https://github.com/hanc00l/nemo_go/releases/download/v3.2.1/nemo_linux_amd64.tar \
    && mkdir -p /opt/nemo \
    && tar -xzf /tmp/nemo-official.tar -C /opt/nemo thirdparty \
    && rm -f /tmp/nemo-official.tar

# Nemo 源码（仓库 thirdparty 内仅数据文件与 .gitkeep，并入上一步解出结果不会覆盖二进制）
# + Go 编译产物
COPY . /opt/nemo
COPY --from=gobuild /out/ /opt/nemo/

# Worker 运行目录（daemon_worker + 其资源目录）
RUN mkdir -p /opt/nemo_worker/logs /opt/nemo_worker/third_party
COPY docker/server.yml docker/worker.yml /opt/nemo/conf/
COPY --from=gobuild /out/daemon_worker_linux_amd64 /opt/nemo_worker/

# 清理上游遗留的 .sock.lock，并校验工具二进制齐全（缺失则构建失败）
RUN find /opt/nemo/thirdparty -name ".sock.lock" -delete \
    && for t in nuclei subfinder gogo massdns fingerprintx zombie httpx; do \
         f="/opt/nemo/thirdparty/$t/${t}_linux_amd64"; \
         [ -x "$f" ] || { echo "ERR: 缺失 $f"; exit 1; }; \
       done \
    && echo "---- thirdparty 工具清单（官方 v3.2.1 配套版本）:" \
    && ls -lh /opt/nemo/thirdparty/*/[a-z]*_linux_amd64
