#!/usr/bin/env python3
# -*- coding: utf-8 -*-

import sys
import argparse
import time
import oci


def rotate_ip(config_file, profile, instance_id):
    print(f"[+] 开始执行换 IP 任务...")
    print(f"    配置文件: {config_file}")
    print(f"    Profile:  {profile}")
    print(f"    实例ID:   {instance_id}")

    # 1. 读取对应 profile 的 OCI 配置
    try:
        config = oci.config.from_file(config_file, profile)
    except Exception as e:
        raise Exception(f"读取 OCI 配置文件失败 [Profile: {profile}]: {e}")

    compute = oci.core.ComputeClient(config)
    network = oci.core.VirtualNetworkClient(config)

    # 2. 获取实例信息与 Compartment ID
    print("[+] 获取实例信息...")
    instance = compute.get_instance(instance_id).data
    compartment_id = instance.compartment_id
    print(f"    Compartment: {compartment_id}")

    # 3. 获取 VNIC
    print("[+] 获取 VNIC...")
    attachments = compute.list_vnic_attachments(
        compartment_id=compartment_id,
        instance_id=instance_id
    ).data

    if not attachments:
        raise Exception("没有找到对应实例的 VNIC")

    vnic_id = attachments[0].vnic_id
    print(f"    VNIC ID: {vnic_id}")

    # 4. 获取 Private IP
    print("[+] 获取 Private IP...")
    private_ips = network.list_private_ips(vnic_id=vnic_id).data
    if not private_ips:
        raise Exception("没有找到对应 VNIC 的 Private IP")

    private_ip = private_ips[0]
    print(f"    Private IP: {private_ip.ip_address}")

    # 5. 获取当前 Ephemeral Public IP
    print("[+] 获取当前 Ephemeral Public IP...")
    public_ip_details = oci.core.models.GetPublicIpByPrivateIpIdDetails(
        private_ip_id=private_ip.id
    )

    public_ip = network.get_public_ip_by_private_ip_id(public_ip_details).data
    print(f"    [+] 当前旧公网 IP: {public_ip.ip_address}")

    # 6. 释放旧公网 IP
    print("[+] 释放旧 Ephemeral Public IP...")
    network.delete_public_ip(public_ip.id)

    print("[+] 等待 15 秒以确保释放完成...")
    time.sleep(15)

    # 7. 创建新的 Ephemeral Public IP
    print("[+] 创建新的 Ephemeral Public IP...")
    new_public_ip = network.create_public_ip(
        oci.core.models.CreatePublicIpDetails(
            compartment_id=compartment_id,
            lifetime="EPHEMERAL",
            private_ip_id=private_ip.id
        )
    ).data

    print("")
    print("==========================================")
    print(f"[SUCCESS] 换 IP 成功！新公网 IP: {new_public_ip.ip_address}")
    print("==========================================")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Oracle Cloud OCI 自动更换临时公网 IP 脚本 (多账号/多实例通用版)")
    parser.add_argument("--profile", default="DEFAULT", help="OCI 配置文件中的 Profile 名称 (默认: DEFAULT)")
    parser.add_argument("--instance-id", required=True, help="Oracle VPS 实例的 OCID")
    parser.add_argument("--config-file", default="/home/ubuntu/.oci/config", help="OCI 配置文件路径 (默认: /home/ubuntu/.oci/config)")

    args = parser.parse_args()

    try:
        rotate_ip(args.config_file, args.profile, args.instance_id)
    except Exception as e:
        print(f"[-] [ERROR] 换 IP 失败: {e}", file=sys.stderr)
        sys.exit(1)
