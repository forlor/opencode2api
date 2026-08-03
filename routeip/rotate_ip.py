#!/usr/bin/env python3
# -*- coding: utf-8 -*-

import oci
import time


# ==========================
# 配置
# ==========================

CONFIG_FILE = "/home/ubuntu/.oci/config"
PROFILE = "DEFAULT"

# 修改为你的实例 OCID
INSTANCE_ID = "ocid1.instance.oc1.phx.anyhqljtopuuszyczmgvid4xbt6kr5x57t7t3msvuyvxdyhsz3r3msrpee3a"


# ==========================
# 初始化
# ==========================

config = oci.config.from_file(
    CONFIG_FILE,
    PROFILE
)

compute = oci.core.ComputeClient(config)
network = oci.core.VirtualNetworkClient(config)


# ==========================
# 获取实例
# ==========================

print("[+] 获取实例信息...")

instance = compute.get_instance(
    INSTANCE_ID
).data

compartment_id = instance.compartment_id

print(
    "Compartment:",
    compartment_id
)


# ==========================
# 获取 VNIC
# ==========================

print("[+] 获取 VNIC...")

attachments = compute.list_vnic_attachments(
    compartment_id=compartment_id,
    instance_id=INSTANCE_ID
).data

if not attachments:
    raise Exception("没有找到 VNIC")

vnic_id = attachments[0].vnic_id

print(
    "VNIC:",
    vnic_id
)


# ==========================
# 获取 Private IP
# ==========================

print("[+] 获取 Private IP...")

private_ips = network.list_private_ips(
    vnic_id=vnic_id
).data

if not private_ips:
    raise Exception("没有找到 Private IP")

private_ip = private_ips[0]

print(
    "Private IP:",
    private_ip.ip_address
)


# ==========================
# 获取当前 Ephemeral Public IP
# ==========================

print("[+] 获取当前 Public IP...")

public_ip_details = oci.core.models.GetPublicIpByPrivateIpIdDetails(
    private_ip_id=private_ip.id
)

public_ip = network.get_public_ip_by_private_ip_id(
    public_ip_details
).data

print(
    "[+] 当前公网 IP:",
    public_ip.ip_address
)


# ==========================
# 释放旧公网 IP
# ==========================

print("[+] 释放 Ephemeral Public IP...")

network.delete_public_ip(
    public_ip.id
)

print("[+] 等待释放...")

time.sleep(15)


# ==========================
# 创建新的 Ephemeral Public IP
# ==========================

print("[+] 创建新的 Ephemeral Public IP...")

new_public_ip = network.create_public_ip(
    oci.core.models.CreatePublicIpDetails(
        compartment_id=compartment_id,
        lifetime="EPHEMERAL",
        private_ip_id=private_ip.id
    )
).data

print("")
print("==========================")
print("新的公网 IP:")
print(new_public_ip.ip_address)
print("==========================")
