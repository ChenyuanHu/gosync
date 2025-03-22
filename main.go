package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 定义全局变量
var (
	targetDir      string                // 要同步的目录
	peerIPs        []string              // 对等节点的IP地址列表
	port           int                   // 服务器监听端口
	fileList       = make(map[string]bool) // 当前节点的文件列表
	fileListMux    sync.RWMutex          // 文件列表的互斥锁
	wg             sync.WaitGroup        // 等待所有同步完成
	syncCompleted  = false               // 同步完成标志
	syncMux        sync.RWMutex          // 同步状态互斥锁
	syncAttempts   = 0                   // 同步尝试次数
	maxAttempts    = 5                   // 最大同步尝试次数
	ignoredNodes   = make(map[string]bool) // 记录无法连接的节点
	ignoredNodesMu sync.RWMutex          // 忽略节点的互斥锁
	syncDelay      int                   // 同步完成后的等待时间（秒）
)

func main() {
	// 解析命令行参数
	flag.StringVar(&targetDir, "dir", "", "要同步的目录路径")
	flag.IntVar(&port, "port", 18283, "服务器监听端口")
	flag.IntVar(&syncDelay, "delay", 10, "同步完成后等待时间（秒）")
	var peers string
	flag.StringVar(&peers, "peers", "", "对等节点的IP地址，用逗号分隔")
	flag.Parse()

	if targetDir == "" {
		log.Fatal("必须指定要同步的目录路径")
	}

	if peers != "" {
		peerIPs = strings.Split(peers, ",")
	}

	// 确保目标目录存在
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		log.Fatalf("创建目录失败: %v", err)
	}

	// 获取当前节点的文件列表
	if err := filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			// 获取相对路径
			relPath, err := filepath.Rel(targetDir, path)
			if err != nil {
				return err
			}
			fileListMux.Lock()
			fileList[relPath] = true
			fileListMux.Unlock()
		}
		return nil
	}); err != nil {
		log.Fatalf("遍历目录失败: %v", err)
	}

	log.Printf("本地文件列表: %v", getFileListKeys())

	// 启动HTTP服务器
	http.HandleFunc("/files", handleFileList)
	http.HandleFunc("/file", handleFileDownload)
	http.HandleFunc("/sync", handleSyncRequest)
	http.HandleFunc("/check", handleCheckSync)

	// 在后台启动服务器
	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}
	go func() {
		log.Printf("服务器在端口 %d 上启动", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务器启动失败: %v", err)
		}
	}()

	// 给服务器一些启动时间
	time.Sleep(1 * time.Second)

	// 如果没有对等节点，程序可以直接退出
	if len(peerIPs) == 0 {
		log.Println("没有指定对等节点，退出程序")
		return
	}

	// 开始同步循环
	for syncAttempts < maxAttempts {
		syncAttempts++
		log.Printf("开始第 %d 次同步尝试", syncAttempts)
		
		// 与每个对等节点同步
		for _, ip := range peerIPs {
			wg.Add(1)
			go syncWithPeer(ip)
		}

		// 等待所有同步完成
		wg.Wait()
		
		// 检查是否所有节点都同步完成
		if checkAllNodesInSync() {
			log.Printf("所有节点文件已同步，等待 %d 秒后退出程序...", syncDelay)
			// 给其他节点一些时间来确认同步
			time.Sleep(time.Duration(syncDelay) * time.Second)
			log.Println("退出程序")
			return
		}
		
		// 等待一段时间后重新尝试
		log.Println("同步未完成，等待后重试...")
		time.Sleep(3 * time.Second)
	}
	
	log.Printf("达到最大尝试次数 %d，退出程序", maxAttempts)
}

// 检查指定节点是否应被忽略
func shouldIgnoreNode(ip string) bool {
	ignoredNodesMu.RLock()
	defer ignoredNodesMu.RUnlock()
	return ignoredNodes[ip]
}

// 将节点标记为忽略
func ignoreNode(ip string) {
	ignoredNodesMu.Lock()
	defer ignoredNodesMu.Unlock()
	ignoredNodes[ip] = true
	log.Printf("忽略无法连接的节点: %s", ip)
}

// 检查所有节点是否都同步完成
func checkAllNodesInSync() bool {
	accessibleNodeCount := 0
	totalNodeCount := len(peerIPs)
	
	// 筛选出可访问的节点列表
	for _, ip := range peerIPs {
		// 跳过已标记为忽略的节点
		if shouldIgnoreNode(ip) {
			continue
		}
		
		peerURL := fmt.Sprintf("http://%s:%d", ip, port)
		
		// 创建带超时的HTTP客户端
		client := &http.Client{
			Timeout: 5 * time.Second, // 缩短超时时间，加快检测
		}
		
		// 获取对等节点的文件列表
		resp, err := client.Get(peerURL + "/files")
		if err != nil {
			log.Printf("无法连接到节点 %s 检查同步状态: %v", peerURL, err)
			// 将无法连接的节点标记为忽略
			ignoreNode(ip)
			continue
		}
		
		// 读取对等节点的文件列表
		peerFiles := make(map[string]bool)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			peerFiles[scanner.Text()] = true
		}
		resp.Body.Close()
		
		if err := scanner.Err(); err != nil {
			log.Printf("读取节点 %s 的文件列表失败: %v", peerURL, err)
			continue
		}
		
		// 比较文件列表
		fileListMux.RLock()
		filesMatch := true
		if len(peerFiles) != len(fileList) {
			filesMatch = false
		} else {
			for file := range fileList {
				if !peerFiles[file] {
					filesMatch = false
					break
				}
			}
		}
		fileListMux.RUnlock()
		
		if filesMatch {
			accessibleNodeCount++
		}
	}
	
	ignoredNodesMu.RLock()
	ignoredCount := len(ignoredNodes)
	ignoredNodesMu.RUnlock()
	
	log.Printf("节点状态: 总节点数=%d, 可访问节点数=%d, 已忽略节点数=%d", totalNodeCount, accessibleNodeCount, ignoredCount)
	
	// 如果所有未忽略的节点都同步完成，则认为同步成功
	return accessibleNodeCount + ignoredCount >= totalNodeCount
}

// 处理检查同步状态请求
func handleCheckSync(w http.ResponseWriter, r *http.Request) {
	syncMux.RLock()
	completed := syncCompleted
	syncMux.RUnlock()
	
	if completed {
		w.Write([]byte("COMPLETED"))
	} else {
		w.Write([]byte("PENDING"))
	}
}

// 获取文件列表的键
func getFileListKeys() []string {
	fileListMux.RLock()
	defer fileListMux.RUnlock()
	
	keys := make([]string, 0, len(fileList))
	for k := range fileList {
		keys = append(keys, k)
	}
	return keys
}

// 处理文件列表请求
func handleFileList(w http.ResponseWriter, r *http.Request) {
	fileListMux.RLock()
	defer fileListMux.RUnlock()
	
	for file := range fileList {
		fmt.Fprintln(w, file)
	}
}

// 处理文件下载请求
func handleFileDownload(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("name")
	if filename == "" {
		http.Error(w, "文件名不能为空", http.StatusBadRequest)
		return
	}

	filePath := filepath.Join(targetDir, filename)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		http.Error(w, "文件不存在", http.StatusNotFound)
		return
	}

	file, err := os.Open(filePath)
	if err != nil {
		http.Error(w, fmt.Sprintf("无法打开文件: %v", err), http.StatusInternalServerError)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filepath.Base(filename)))
	w.Header().Set("Content-Type", "application/octet-stream")

	if _, err := io.Copy(w, file); err != nil {
		http.Error(w, fmt.Sprintf("传输文件失败: %v", err), http.StatusInternalServerError)
		return
	}
}

// 处理同步请求
func handleSyncRequest(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("file")
	if filename == "" {
		http.Error(w, "文件名不能为空", http.StatusBadRequest)
		return
	}

	// 检查文件是否已存在
	fileListMux.RLock()
	_, exists := fileList[filename]
	fileListMux.RUnlock()

	if exists {
		w.Write([]byte("文件已存在"))
		return
	}

	// 解析URL中的远程节点地址
	remoteIP := r.RemoteAddr
	// 去除端口部分
	if idx := strings.LastIndex(remoteIP, ":"); idx != -1 {
		remoteIP = remoteIP[:idx]
	}
	// 移除IPv6地址的方括号
	remoteIP = strings.Trim(remoteIP, "[]")
	
	remoteURL := fmt.Sprintf("http://%s:%d", remoteIP, port)

	// 异步下载文件
	go func() {
		if err := downloadFile(remoteURL, filename); err != nil {
			log.Printf("从节点 %s 下载文件 %s 失败: %v", remoteURL, filename, err)
			return
		}

		// 添加到我的文件列表
		fileListMux.Lock()
		fileList[filename] = true
		fileListMux.Unlock()
		
		log.Printf("从节点 %s 成功下载文件 %s", remoteURL, filename)
	}()

	w.Write([]byte("开始下载文件"))
}

// 与对等节点同步
func syncWithPeer(ip string) {
	defer wg.Done()
	
	peerURL := fmt.Sprintf("http://%s:%d", ip, port)
	log.Printf("与节点 %s 开始同步", peerURL)

	// 创建带超时的HTTP客户端
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// 获取对等节点的文件列表
	resp, err := client.Get(peerURL + "/files")
	if err != nil {
		log.Printf("无法连接到节点 %s: %v", peerURL, err)
		return
	}
	defer resp.Body.Close()

	// 读取对等节点的文件列表
	peerFiles := make(map[string]bool)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		peerFiles[scanner.Text()] = true
	}

	if err := scanner.Err(); err != nil {
		log.Printf("读取节点 %s 的文件列表失败: %v", peerURL, err)
		return
	}

	log.Printf("节点 %s 的文件列表: %v", peerURL, peerFiles)

	// 找出我有对方没有的文件
	filesToPush := []string{}
	fileListMux.RLock()
	for file := range fileList {
		if !peerFiles[file] {
			filesToPush = append(filesToPush, file)
		}
	}
	fileListMux.RUnlock()

	// 找出对方有我没有的文件
	filesToPull := []string{}
	for file := range peerFiles {
		fileListMux.RLock()
		hasFile := fileList[file]
		fileListMux.RUnlock()
		
		if !hasFile {
			filesToPull = append(filesToPull, file)
		}
	}

	log.Printf("将向节点 %s 推送的文件: %v", peerURL, filesToPush)
	log.Printf("将从节点 %s 拉取的文件: %v", peerURL, filesToPull)

	// 下载对方有我没有的文件
	for _, file := range filesToPull {
		if err := downloadFile(peerURL, file); err != nil {
			log.Printf("从节点 %s 下载文件 %s 失败: %v", peerURL, file, err)
			continue
		}

		// 添加到我的文件列表
		fileListMux.Lock()
		fileList[file] = true
		fileListMux.Unlock()
		
		log.Printf("从节点 %s 成功下载文件 %s", peerURL, file)
	}

	// 通知对方下载我有它没有的文件
	for _, file := range filesToPush {
		if _, err := client.Get(fmt.Sprintf("%s/sync?file=%s", peerURL, file)); err != nil {
			log.Printf("通知节点 %s 下载文件 %s 失败: %v", peerURL, file, err)
		}
	}

	log.Printf("与节点 %s 同步完成", peerURL)
}

// 从对等节点下载文件
func downloadFile(peerURL, filename string) error {
	// 创建带超时的HTTP客户端
	client := &http.Client{
		Timeout: 60 * time.Second, // 较长的超时时间用于文件下载
	}
	
	resp, err := client.Get(fmt.Sprintf("%s/file?name=%s", peerURL, filename))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("服务器返回错误: %d", resp.StatusCode)
	}

	// 创建文件所在的目录
	filePath := filepath.Join(targetDir, filename)
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// 创建文件
	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	// 写入文件内容
	_, err = io.Copy(file, resp.Body)
	return err
} 