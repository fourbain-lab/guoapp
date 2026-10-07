package core

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// newHongguoRefreshTestDownloader 造一个带真实限流器的 Downloader。
// 生产构造函数在 app_runtime.go 里总会设置 limiter，测试必须对齐，
// 否则 refresh 路径会在 requestLimiter.acquire 上空指针崩溃。
func newHongguoRefreshTestDownloader() *Downloader {
	return &Downloader{
		cfg:     Config{},
		limiter: newRequestLimiter(3, 250*time.Millisecond),
	}
}

// TestRefreshHongguoMediaURLReturnsErrorOnEmptyID 边界条件
func TestRefreshHongguoMediaURLReturnsErrorOnEmptyID(t *testing.T) {
	d := newHongguoRefreshTestDownloader()
	if _, err := d.refreshHongguoMediaURL(context.Background(), "", ""); err == nil {
		t.Fatal("expected error for empty seriesID/videoID, got nil")
	}
	_, err := d.refreshHongguoMediaURL(context.Background(), "123", "")
	if err == nil || !strings.Contains(err.Error(), "剧集 ID") {
		t.Fatal("error message should mention 剧集 ID")
	}
}

// TestRefreshHongguoMediaURLPropagatesPlaybackError refresh 路径走 resolveHongguoPlaybackAPI，
// 空 ctx 应传播错误
func TestRefreshHongguoMediaURLPropagatesPlaybackError(t *testing.T) {
	d := newHongguoRefreshTestDownloader()
	d.cfg.HongguoURL = "http://127.0.0.1:1/" // unreachable
	d.client = &http.Client{Transport: errTransport{err: errors.New("playback api down")}}
	_, err := d.refreshHongguoMediaURL(context.Background(), "123", "456")
	if err == nil {
		t.Fatal("expected playback API error to propagate")
	}
}

// errTransport 总是返回 err 的 transport（用于验证错误传播）
type errTransport struct{ err error }

func (t errTransport) RoundTrip(r *http.Request) (*http.Response, error) { return nil, t.err }
