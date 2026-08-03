#!/usr/bin/env bash

# 更换 VPS 临时公网 IP 脚本模版
# 当节点收到 FreeUsageLimitError (429) 时，Go 主服务会自动触发执行本脚本

set -e

echo "[$(date '+%Y-%m-%d %H:%M:%S')] 收到更换公网 IP 请求..."

# 示例 1: 调用 Oracle Cloud CLI / 云服务商 API 更换 VNIC 的临时公网 IP
# oci network public-ip update --public-ip-id <OCID> --lifetime "EPHEMERAL" ...

# 示例 2: 重新拨号 / 重置网卡 (示例)
# ifdown eth0 && ifup eth0

echo "[$(date '+%Y-%m-%d %H:%M:%S')] 更换公网 IP 指令已发送，脚本执行完成。"
