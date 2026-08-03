# OpenCode2API Linux ARM64 部署指南

本指南适用于将 **OpenCode2API** 主服务与 Nginx 子节点部署在 **Linux ARM64** 架构环境（如 Oracle Cloud 4C24G/1C1G ARM 实例、树莓派等）。

---

## 一、 架构与部署准备

- **主服务 (Gateway)**：Go 语言编写，单文件静态编译，内存占用低（< 20MB），支持交叉编译直接部署在 ARM64 节点上。
- **子节点 (Nginx Sub-node)**：Oracle VPS 节点上运行轻量 Nginx 局域网代理，负责脱敏主服务 IP 并与 `opencode.ai` 通信。

---

## 二、 编译 ARM64 二进制文件

您可以在本地（Windows/Mac/Linux）使用 Go 交叉编译出适用于 Linux ARM64 架构的静独立二进制可执行文件：

### 1. 本地交叉编译 (Cross Compilation)

#### 在 Windows PowerShell / CMD 中编译：
```bash
# PowerShell
$env:GOOS="linux"
$env:GOARCH="arm64"
$env:CGO_ENABLED="0"
go build -ldflags="-w -s" -o opencode2api-linux-arm64 main.go

# Cmd
set GOOS=linux
set GOARCH=arm64
set CGO_ENABLED=0
go build -ldflags="-w -s" -o opencode2api-linux-arm64 main.go
```

#### 在 Linux / macOS 中编译：
```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-w -s" -o opencode2api-linux-arm64 main.go
```

---

## 三、 子节点 Nginx 配置 (每个 Oracle VPS ARM 节点)

在各个 VPS 子节点上安装 Nginx（Ubuntu/Debian/CentOS）：

```bash
# Ubuntu / Debian
sudo apt update && sudo apt install -y nginx

# CentOS / RHEL
sudo yum install -y nginx
```

修改 `/etc/nginx/sites-available/default` 或 `/etc/nginx/conf.d/opencode.conf`：

```nginx
server {
    # 绑定局域网/内网端口，仅内网可访问
    listen 8080;
    server_name _;

    location / {
        # 1. 验证来自 Go 主服务的 Secret 请求头 (与 config.yaml 中的 secret 对应)
        if ($http_x_proxy_secret != "your-lan-proxy-secret-123456") {
            return 403 "Forbidden";
        }

        # 2. 代理至 opencode.ai
        proxy_pass https://opencode.ai/;
        proxy_ssl_server_name on;
        proxy_ssl_name opencode.ai;

        # 3. 禁用缓冲区以支持 SSE 流式推流
        proxy_buffering off;
        proxy_cache off;

        # 4. 仅保留目标 Host，绝对不透传 X-Forwarded-For 或 X-Real-IP
        proxy_set_header Host opencode.ai;
        proxy_set_header Connection '';
        proxy_http_version 1.1;
    }
}
```

测试并重载 Nginx：
```bash
sudo nginx -t
sudo nginx -s reload
```

---

## 四、 主服务配置与运行

### 1. 准备目录与配置文件
在主服务机器上创建部署目录：

```bash
mkdir -p /opt/opencode2api/scripts
cd /opt/opencode2api
```

创建 `config.yaml` 配置文件：

```yaml
server:
  port: 8080
  api_keys:
    - "sk-opencode-secret-key-1" # 客户端请求接口用的 API Key
  secret: "your-lan-proxy-secret-123456" # 局域网 Nginx 校验秘钥

default:
  cooldown_duration: "30m" # 不可换 IP 节点的默认 429 冷却时间
  model_mapping: "deepseek-v4-flash-free"

nodes:
  - name: "vps-node-01"
    lan_url: "http://192.168.1.101:8080" # VPS 1 局域网 IP
    supports_ip_change: true
    ip_change_command: "bash /opt/opencode2api/scripts/change_ip_vps1.sh"
    cooldown_duration: "30m"

  - name: "vps-node-02"
    lan_url: "http://192.168.1.102:8080" # VPS 2 局域网 IP (固定 IP 节点)
    supports_ip_change: false
    cooldown_duration: "30m"
```

### 2. 换公网 IP 脚本授权
如果节点支持更换临时公网 IP，将脚本放入 `/opt/opencode2api/scripts/` 并赋予可执行权限：

```bash
chmod +x /opt/opencode2api/scripts/change_ip_*.sh
```

### 3. 使用 Systemd 管理服务进程

创建 `/etc/systemd/system/opencode2api.service` 服务文件：

```ini
[Unit]
Description=OpenCode2API Gateway Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/opencode2api
ExecStart=/opt/opencode2api/opencode2api-linux-arm64 /opt/opencode2api/config.yaml
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

启动并设置开机自启：
```bash
sudo systemctl daemon-reload
sudo systemctl enable opencode2api
sudo systemctl start opencode2api

# 查看运行状态与日志
sudo systemctl status opencode2api
sudo journalctl -u opencode2api -f
```

---

## 五、 Docker / Docker Compose 部署方式 (ARM64 架构)

如果您希望在 ARM64 环境下直接使用 Docker 部署，项目自带的 `Dockerfile` 支持多阶段构建，可以直接构建出兼容 ARM64 的轻量镜像：

```bash
# 1. 在 ARM64 机器上构建 Docker 镜像
docker build -t opencode2api:latest .

# 2. 使用 Docker Compose 启动
docker-compose up -d
```

`docker-compose.yml` 内容：
```yaml
version: '3.8'

services:
  opencode2api:
    image: opencode2api:latest
    container_name: opencode2api
    restart: always
    ports:
      - "8080:8080"
    volumes:
      - ./config.yaml:/app/config.yaml
      - ./scripts:/app/scripts
    environment:
      - TZ=Asia/Shanghai
```

---

## 六、 部署验证与监控 API

### 1. 验证 OpenAI 兼容对话端点
```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-opencode-secret-key-1" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "deepseek-chat",
    "stream": true,
    "messages": [
      {"role": "user", "content": "你好"}
    ]
  }'
```

### 2. 查看各 VPS 节点实时监控状态
访问监控接口：
```bash
curl http://localhost:8080/admin/nodes
```

**响应示例**：
```json
{
  "total_nodes": 2,
  "nodes": [
    {
      "name": "vps-node-01",
      "lan_url": "http://192.168.1.101:8080",
      "status": "Active",
      "session_id": "ses_4a1f8c02...",
      "next_session_rotate": "14:35:10",
      "supports_ip_change": true,
      "total_requests": 128,
      "success_requests": 126,
      "failed_requests": 2,
      "ip_change_count": 1
    },
    {
      "name": "vps-node-02",
      "lan_url": "http://192.168.1.102:8080",
      "status": "Cooling",
      "cooling_until": "2026-08-03T15:10:00Z",
      "session_id": "ses_8b2e31ff...",
      "next_session_rotate": "14:52:00",
      "supports_ip_change": false,
      "total_requests": 95,
      "success_requests": 90,
      "failed_requests": 5,
      "ip_change_count": 0
    }
  ]
}
```
