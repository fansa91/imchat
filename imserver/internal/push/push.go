package push

import (
	"context"
	"log/slog"
)

// Notifier 离线推送接口。后续接入 APNs / FCM / 厂商通道时实现此接口即可。
type Notifier interface {
	Notify(ctx context.Context, userID uint64, title, body string, data map[string]string) error
}

// LogNotifier 占位实现：只打日志。
type LogNotifier struct{}

func (LogNotifier) Notify(ctx context.Context, userID uint64, title, body string, data map[string]string) error {
	slog.Debug("offline push (stub)", "user", userID, "title", title, "body", body)
	return nil
}
