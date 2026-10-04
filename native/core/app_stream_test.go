package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestRefreshHongguoMediaURLReturnsErrorOnEmptyID 边界条件
func TestRefreshHongguoMediaURLReturnsErrorOnEmptyID(t *testing.T) {
	d := &Downloader{cfg: &Config{}}
	if _, err := d.refreshHongguoMediaURL(context.Background(), "", ""); err == nil {
		t.Fatal("expected error for empty seriesID/videoID, got nil")
	}
	if !strings.Contains(d.refreshHongguoMediaURL(context.Background(), "123", "").Error(), "剧集 ID") {
		t.Fatal("error message should mention 剧集 ID")
	}
}

// TestRefreshHongguoMediaURLPropagatesPlaybackError refresh 路径走 resolveHongguoPlaybackAPI，
// 空 ctx 应传播错误
func TestRefreshHongguoMediaURLPropagatesPlaybackError(t *testing.T) {
	d := &Downloader{cfg: &Config{}}
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