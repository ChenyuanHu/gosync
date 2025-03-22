#!/usr/bin/env python3
# -*- coding: utf-8 -*-

import argparse
import logging
import os
import threading
import time
from pathlib import Path
from typing import Dict, List, Set

import requests
from flask import Flask, jsonify, request, send_file
from werkzeug.serving import make_server

# 配置日志
logging.basicConfig(
    format='%(asctime)s %(levelname)s: %(message)s',
    level=logging.INFO,
    datefmt='%Y/%m/%d %H:%M:%S'
)
logger = logging.getLogger(__name__)

# 全局变量
class GlobalState:
    def __init__(self):
        self.target_dir = ""
        self.peer_ips = []
        self.port = 18283
        self.sync_delay = 10
        self.file_list = set()
        self.file_list_lock = threading.RLock()
        self.ignored_nodes = set()
        self.ignored_nodes_lock = threading.RLock()
        self.sync_attempts = 0
        self.max_attempts = 5

# 初始化全局状态
state = GlobalState()

# 初始化Flask应用
app = Flask(__name__)
server = None

# 线程安全的添加文件到文件列表
def add_file_to_list(filename):
    with state.file_list_lock:
        state.file_list.add(filename)

# 获取目录下所有文件的相对路径
def scan_directory(directory: str) -> Set[str]:
    files = set()
    directory_path = Path(directory)
    
    for file_path in directory_path.glob('**/*'):
        if file_path.is_file():
            rel_path = str(file_path.relative_to(directory_path))
            # 统一使用正斜杠，确保跨平台一致性
            rel_path = rel_path.replace('\\', '/')
            files.add(rel_path)
    
    return files

# 下载文件
def download_file(peer_url: str, filename: str) -> bool:
    try:
        # 添加超时处理
        response = requests.get(f"{peer_url}/file", params={"name": filename}, timeout=60)
        if response.status_code != 200:
            logger.error(f"从节点 {peer_url} 下载文件 {filename} 失败: 状态码 {response.status_code}")
            return False
        
        # 创建目标路径
        file_path = Path(state.target_dir) / filename
        file_path.parent.mkdir(parents=True, exist_ok=True)
        
        # 写入文件内容
        with open(file_path, 'wb') as f:
            f.write(response.content)
        
        logger.info(f"从节点 {peer_url} 成功下载文件 {filename}")
        add_file_to_list(filename)
        return True
    except Exception as e:
        logger.error(f"从节点 {peer_url} 下载文件 {filename} 失败: {str(e)}")
        return False

# 检查节点是否应被忽略
def should_ignore_node(ip: str) -> bool:
    with state.ignored_nodes_lock:
        return ip in state.ignored_nodes

# 标记节点为忽略
def ignore_node(ip: str):
    with state.ignored_nodes_lock:
        state.ignored_nodes.add(ip)
        logger.info(f"忽略无法连接的节点: {ip}")

# 与对等节点同步
def sync_with_peer(ip: str):
    peer_url = f"http://{ip}:{state.port}"
    logger.info(f"与节点 {peer_url} 开始同步")
    
    try:
        # 获取对等节点的文件列表
        response = requests.get(f"{peer_url}/files", timeout=30)
        if response.status_code != 200:
            logger.error(f"无法获取节点 {peer_url} 的文件列表: 状态码 {response.status_code}")
            return
        
        # 解析对等节点文件列表
        peer_files = set(response.text.splitlines())
        logger.info(f"节点 {peer_url} 的文件列表: {peer_files}")
        
        # 获取本地文件列表
        with state.file_list_lock:
            local_files = state.file_list.copy()
        
        # 找出需要推送和拉取的文件
        files_to_push = local_files - peer_files
        files_to_pull = peer_files - local_files
        
        logger.info(f"将向节点 {peer_url} 推送的文件: {files_to_push}")
        logger.info(f"将从节点 {peer_url} 拉取的文件: {files_to_pull}")
        
        # 下载对方有我没有的文件
        for file in files_to_pull:
            download_file(peer_url, file)
        
        # 通知对方下载我有它没有的文件
        for file in files_to_push:
            try:
                requests.get(f"{peer_url}/sync", params={"file": file}, timeout=30)
            except Exception as e:
                logger.error(f"通知节点 {peer_url} 下载文件 {file} 失败: {str(e)}")
        
        logger.info(f"与节点 {peer_url} 同步完成")
    except Exception as e:
        logger.error(f"与节点 {peer_url} 同步失败: {str(e)}")

# 检查所有节点是否同步完成
def check_all_nodes_in_sync() -> bool:
    accessible_node_count = 0
    total_node_count = len(state.peer_ips)
    
    for ip in state.peer_ips:
        # 跳过已标记为忽略的节点
        if should_ignore_node(ip):
            continue
        
        peer_url = f"http://{ip}:{state.port}"
        
        try:
            # 获取对等节点的文件列表
            response = requests.get(f"{peer_url}/files", timeout=5)
            if response.status_code != 200:
                logger.error(f"无法获取节点 {peer_url} 的文件列表: 状态码 {response.status_code}")
                continue
            
            # 解析对等节点文件列表
            peer_files = set(response.text.splitlines())
            
            # 比较文件列表
            with state.file_list_lock:
                files_match = peer_files == state.file_list
            
            if files_match:
                accessible_node_count += 1
            
        except Exception as e:
            logger.error(f"无法连接到节点 {peer_url} 检查同步状态: {str(e)}")
            ignore_node(ip)
    
    with state.ignored_nodes_lock:
        ignored_count = len(state.ignored_nodes)
    
    logger.info(f"节点状态: 总节点数={total_node_count}, 可访问节点数={accessible_node_count}, 已忽略节点数={ignored_count}")
    
    # 如果所有未忽略的节点都同步完成，则认为同步成功
    return accessible_node_count + ignored_count >= total_node_count

# Flask路由
@app.route('/files', methods=['GET'])
def handle_file_list():
    """返回文件列表"""
    with state.file_list_lock:
        return '\n'.join(state.file_list)

@app.route('/file', methods=['GET'])
def handle_file_download():
    """处理文件下载请求"""
    filename = request.args.get('name', '')
    if not filename:
        return "文件名不能为空", 400
    
    file_path = os.path.join(state.target_dir, filename)
    if not os.path.exists(file_path):
        return "文件不存在", 404
    
    return send_file(file_path, as_attachment=True)

@app.route('/sync', methods=['GET'])
def handle_sync_request():
    """处理同步请求"""
    filename = request.args.get('file', '')
    if not filename:
        return "文件名不能为空", 400
    
    # 检查文件是否已存在
    with state.file_list_lock:
        exists = filename in state.file_list
    
    if exists:
        return "文件已存在"
    
    # 获取远程节点地址
    remote_ip = request.remote_addr
    remote_url = f"http://{remote_ip}:{state.port}"
    
    # 异步下载文件
    def download_async():
        if download_file(remote_url, filename):
            with state.file_list_lock:
                state.file_list.add(filename)
    
    threading.Thread(target=download_async).start()
    return "开始下载文件"

@app.route('/check', methods=['GET'])
def handle_check_sync():
    """处理检查同步状态请求"""
    # 这个端点可以用于将来的扩展
    return "PENDING"

# 主函数
def main():
    parser = argparse.ArgumentParser(description='GoSync Python版 - 多机文件同步工具')
    parser.add_argument('-dir', dest='dir', required=True, help='要同步的目录路径')
    parser.add_argument('-peers', dest='peers', help='对等节点的IP地址，用逗号分隔')
    parser.add_argument('-port', dest='port', type=int, default=18283, help='服务器监听端口 (默认: 18283)')
    parser.add_argument('-delay', dest='delay', type=int, default=10, help='同步完成后等待时间，单位秒 (默认: 10)')
    
    args = parser.parse_args()
    
    # 设置全局状态
    state.target_dir = args.dir
    state.port = args.port
    state.sync_delay = args.delay
    
    if args.peers:
        state.peer_ips = args.peers.split(',')
    
    # 确保目标目录存在
    os.makedirs(state.target_dir, exist_ok=True)
    
    # 扫描目录并建立文件列表
    state.file_list = scan_directory(state.target_dir)
    logger.info(f"本地文件列表: {state.file_list}")
    
    # 启动HTTP服务器
    global server
    server = make_server('0.0.0.0', state.port, app, threaded=True)
    server_thread = threading.Thread(target=server.serve_forever)
    server_thread.daemon = True
    server_thread.start()
    logger.info(f"服务器在端口 {state.port} 上启动")
    
    # 给服务器一些启动时间
    time.sleep(1)
    
    # 如果没有对等节点，程序可以直接退出
    if not state.peer_ips:
        logger.info("没有指定对等节点，退出程序")
        return
    
    # 开始同步循环
    while state.sync_attempts < state.max_attempts:
        state.sync_attempts += 1
        logger.info(f"开始第 {state.sync_attempts} 次同步尝试")
        
        # 创建线程池来与每个对等节点同步
        threads = []
        for ip in state.peer_ips:
            thread = threading.Thread(target=sync_with_peer, args=(ip,))
            thread.start()
            threads.append(thread)
        
        # 等待所有同步线程完成
        for thread in threads:
            thread.join()
        
        # 检查是否所有节点都同步完成
        if check_all_nodes_in_sync():
            logger.info(f"所有节点文件已同步，等待 {state.sync_delay} 秒后退出程序...")
            # 给其他节点一些时间来确认同步
            time.sleep(state.sync_delay)
            logger.info("退出程序")
            return
        
        # 等待一段时间后重新尝试
        logger.info("同步未完成，等待后重试...")
        time.sleep(3)
    
    logger.info(f"达到最大尝试次数 {state.max_attempts}，退出程序")

if __name__ == "__main__":
    try:
        main()
    finally:
        # 确保服务器正确关闭
        if server:
            server.shutdown() 