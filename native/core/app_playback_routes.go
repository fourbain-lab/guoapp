package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"time"
)

const nativePlaybackLimit = 8

type nativePlaybackChoice struct {
	danmakuSeries string
	danmakuVideo  string
	media         []providerMedia
	index         int
	qualities     []int
	streamSession string
	created       time.Time
}

func nativePlaybackChoices(media providerMedia, quality int) nativePlaybackChoice {
	choice := nativePlaybackChoice{qualities: []int{}}
	seen := map[string]bool{}
	qualities := map[int]bool{}
	for _, option := range append([]providerMedia{media}, media.Variants...) {
		if !isProviderHTTPMediaURL(option.URL) {
			continue
		}
		identity := option.URL + "\x00" + option.Referer + "\x00" + hex.EncodeToString(option.CENCKey) + "\x00" + hex.EncodeToString(option.HLSKey)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		option.Variants = nil
		choice.media = append(choice.media, option)
		if option.Quality > 0 {
			qualities[option.Quality] = true
		}
	}
	sort.SliceStable(choice.media, func(i, j int) bool { return choice.media[i].Quality > choice.media[j].Quality })
	for value := range qualities {
		choice.qualities = append(choice.qualities, value)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(choice.qualities)))
	if quality > 0 && qualities[quality] {
		filtered := make([]providerMedia, 0, len(choice.media))
		for _, option := range choice.media {
			if option.Quality == quality {
				filtered = append(filtered, option)
			}
		}
		choice.media = filtered
	}
	return choice
}

func (engine *nativeEngine) nativeOpenPlayback(ctx context.Context, choice nativePlaybackChoice) (nativePlan, error) {
	if err := ctx.Err(); err != nil {
		return nativePlan{}, err
	}
	if choice.index < 0 || choice.index >= len(choice.media) {
		return nativePlan{}, errors.New("该集没有其他可用的播放线路")
	}
	media := choice.media[choice.index]
	var err error
	if !isProviderHTTPMediaURL(media.URL) {
		return nativePlan{}, errors.New("站源未返回有效的播放地址")
	}
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nativePlan{}, errors.New("无法初始化播放会话")
	}
	plan := nativePlan{
		DanmakuID: choice.danmakuVideo,
		URL:       media.URL, Headers: map[string]string{"User-Agent": userAgent, "Referer": media.Referer},
		Key: hex.EncodeToString(media.CENCKey), Quality: media.Quality, Qualities: choice.qualities,
		RouteIndex: choice.index, RouteCount: len(choice.media), Session: hex.EncodeToString(tokenBytes),
	}
	if media.credentials != nil && !media.credentials.expires.IsZero() {
		if !time.Now().Before(media.credentials.expires) {
			return nativePlan{}, errors.New("播放凭证已过期，请重新解析播放")
		}
		plan.ExpiresAt = media.credentials.expires.UnixMilli()
	}
	choice.streamSession = ""
	{
		engine.mu.Lock()
		if engine.stream == nil {
			engine.stream, err = newNativeStreamServer(engine.downloader)
		}
		stream := engine.stream
		engine.mu.Unlock()
		if err != nil {
			return nativePlan{}, err
		}
		plan.URL, choice.streamSession = stream.nativeOpen(media)
	}
	choice.created = time.Now()
	engine.mu.Lock()
	if engine.playbacks == nil {
		engine.playbacks = map[string]nativePlaybackChoice{}
	}
	var expired []string
	for token, old := range engine.playbacks {
		if time.Since(old.created) > 12*time.Hour {
			expired = append(expired, old.streamSession)
			delete(engine.playbacks, token)
		}
	}
	if len(engine.playbacks) >= nativePlaybackLimit {
		// 真正被读取过的会话就是正在观看的那一集，必须保留；预加载、重试等
		// 推测性会话从未送出过数据，优先回收。播放器在两次分片请求之间是空闲的，
		// 所以不能以「当前有无请求在途」作为判据。
		victim := ""
		var victimAt time.Time
		for token, old := range engine.playbacks {
			if engine.stream.served(old.streamSession) {
				continue
			}
			if victim == "" || old.created.Before(victimAt) {
				victim, victimAt = token, old.created
			}
		}
		if victim == "" {
			// 全部会话都在使用中（例如多集同时播放）：回收最久未使用的非在用会话。
			var oldestAt time.Time
			for token, old := range engine.playbacks {
				at := old.created
				if used, ok := engine.stream.usedAt(old.streamSession); ok {
					at = used
				}
				if victim == "" || at.Before(oldestAt) {
					victim, oldestAt = token, at
				}
			}
		}
		expired = append(expired, engine.playbacks[victim].streamSession)
		delete(engine.playbacks, victim)
	}
	engine.playbacks[plan.Session] = choice
	stream := engine.stream
	engine.mu.Unlock()
	if stream != nil {
		for _, token := range expired {
			stream.nativeRelease(token)
		}
	}
	if err := ctx.Err(); err != nil {
		engine.nativeReleasePlayback(plan.Session)
		return nativePlan{}, err
	}
	return plan, nil
}

func (engine *nativeEngine) nativeNextPlayback(ctx context.Context, session string) (nativePlan, error) {
	engine.mu.Lock()
	choice, exists := engine.playbacks[session]
	engine.mu.Unlock()
	if !exists {
		return nativePlan{}, errors.New("播放线路已失效，请重新解析播放")
	}
	choice.index++
	return engine.nativeOpenPlayback(ctx, choice)
}

func (engine *nativeEngine) nativeReleasePlayback(session string) {
	engine.mu.Lock()
	choice, exists := engine.playbacks[session]
	delete(engine.playbacks, session)
	stream := engine.stream
	engine.mu.Unlock()
	if exists && stream != nil && choice.streamSession != "" {
		stream.nativeRelease(choice.streamSession)
	}
}
