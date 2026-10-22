package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"imserver/internal/api"
	"imserver/internal/auth"
	"imserver/internal/config"
	"imserver/internal/push"
	"imserver/internal/storage"
	"imserver/internal/store"
	"imserver/internal/stt"
	"imserver/internal/ws"
)

var version = "v0.4.0" // 编译时通过 -ldflags "-X main.version=..." 覆盖

func main() {
	cfgPath := flag.String("c", "config.yaml", "配置文件路径")
	showVersion := flag.Bool("v", false, "显示版本")
	flag.Parse()
	if *showVersion {
		fmt.Println("imserver", version)
		return
	}
	if err := run(*cfgPath); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Log.Level))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.MySQL)
	if err != nil {
		return err
	}
	defer st.Close()
	if cfg.MySQL.AutoMigrate {
		if err := st.Migrate(ctx); err != nil {
			return err
		}
		slog.Info("database migrated")
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	defer rdb.Close()
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pctx).Err(); err != nil {
		return fmt.Errorf("连接 Redis 失败: %w", err)
	}

	stg, err := storage.New(ctx, cfg.Storage)
	if err != nil {
		return err
	}
	if stg == nil {
		slog.Warn("对象存储未启用（storage.enabled=false），语音/图片消息不可用")
	}

	tr := stt.New(cfg.STT)
	switch {
	case tr != nil && stg == nil:
		slog.Warn("stt.enabled=true 但对象存储未启用，语音转文字不可用")
		tr = nil
	case tr != nil:
		slog.Info("语音转文字已启用", "provider", cfg.STT.Provider, "fallback", tr.Config().Fallback, "daily_limit", cfg.STT.DailyLimitPerUser)
	}

	hub := ws.NewHub(rdb, cfg.Server.NodeID)
	go func() {
		if err := hub.Run(ctx); err != nil {
			slog.Error("hub stopped", "err", err)
			stop()
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           api.New(cfg, st, auth.New(cfg.JWT.Secret, cfg.JWT.TTL()), hub, push.LogNotifier{}, stg, tr, rdb).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("imserver listening", "addr", cfg.Server.Addr, "node", cfg.Server.NodeID, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	slog.Info("shutting down")
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	return srv.Shutdown(sctx)
}
