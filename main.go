package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"opencode2api/config"
	"opencode2api/gemini"
	"opencode2api/proxy"
	"opencode2api/server"
)

func main() {
	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Printf("读取配置文件 %s 失败，请检查文件是否存在: %v", configPath, err)
		log.Println("尝试寻找 config.yaml.example 作为基础...")
		if _, err := os.Stat("config.yaml.example"); err == nil {
			log.Println("找到 config.yaml.example，请将其重命名为 config.yaml 并修改配置")
		}
		os.Exit(1)
	}

	log.Printf("成功加载配置，配置节点数: %d，主服务端口: %d", len(cfg.Nodes), cfg.Server.Port)

	// gemini.daily_usage_file 为相对路径时按 config 文件所在目录解析：
	// 否则随进程启动目录漂移（systemd WorkingDirectory 变更/手动 cd 后启动），
	// 用量恢复静默换文件，当日计数与耗尽标记全部丢失
	if cfg.Gemini != nil && cfg.Gemini.DailyUsageFile != "" && !filepath.IsAbs(cfg.Gemini.DailyUsageFile) {
		if absCfg, err := filepath.Abs(configPath); err == nil {
			cfg.Gemini.DailyUsageFile = filepath.Join(filepath.Dir(absCfg), cfg.Gemini.DailyUsageFile)
		}
	}

	// 初始化节点调度池
	pool := proxy.NewPool(cfg)

	// 初始化 Gemini 号池线路（未配置时为空池，路由层恒走 OpenCode）
	var gemPool *gemini.Pool
	if cfg.Gemini != nil {
		gemPool = gemini.NewPool(cfg.Gemini, cfg.Server.Secret)
	}

	// 初始化 HTTP 路由服务
	router := server.NewRouter(cfg, pool, gemPool)

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	log.Printf("OpenCode2API 服务已启动，监听地址: http://%s", addr)
	log.Printf("OpenAI 接口地址: http://%s/v1/chat/completions", addr)
	log.Printf("监控查看 API 地址: http://%s/admin/nodes", addr)
	if gemPool != nil {
		log.Printf("Gemini 线路已启用，节点数: %d，监控地址: http://%s/admin/gemini", gemPool.Len(), addr)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 优雅退出：监听 SIGINT/SIGTERM，等待在途请求处理完成
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务异常退出: %v", err)
		}
	}()
	log.Println("服务已启动，等待退出信号...")

	<-ctx.Done()
	log.Println("收到退出信号，正在优雅关闭...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("优雅关闭失败: %v", err)
	}
	// Gemini 每日用量同步落盘（未启用持久化时为空操作）
	if gemPool != nil {
		gemPool.FlushDailyUsage()
	}
	log.Println("服务已退出")
}
