package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type nativeStreamAsset struct {
	address     string
	data        []byte
	contentType string
	total       int64
	etag        string
	modified    string
}

type nativeStreamSession struct {
	credentials *providerMediaCredentials
	mu          sync.Mutex
	assets      map[string]nativeStreamAsset
	referer     string
	key         []byte
	// v3 fix: 视频过期刷新依据，需要 seriesID/videoID 才能重新解析
	seriesID string
	videoID  string
	ctx      context.Context
	cancel   context.CancelFunc
	lastUsed time.Time
	inflight int
	served   int
}

// served reports whether the session ever delivered media to the player.
func (session *nativeStreamSession) servedCount() int {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.served
}

// inFlight reports whether the session is currently serving a response.
func (session *nativeStreamSession) inFlight() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.inflight > 0
}

// served reports whether the identified session ever delivered media.
func (stream *nativeStreamServer) served(token string) bool {
	if token == "" {
		return false
	}
	stream.mu.Lock()
	session := stream.sessions[token]
	stream.mu.Unlock()
	return session != nil && session.servedCount() > 0
}

// usedAt reports when the identified session last served a response.
func (stream *nativeStreamServer) usedAt(token string) (time.Time, bool) {
	if token == "" {
		return time.Time{}, false
	}
	stream.mu.Lock()
	session := stream.sessions[token]
	stream.mu.Unlock()
	if session == nil {
		return time.Time{}, false
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.lastUsed, true
}

type nativeStreamServer struct {
	mu         sync.Mutex
	downloader *Downloader
	address    string
	sessions   map[string]*nativeStreamSession
	server     *http.Server
	probeMu    sync.Mutex
	probes     []string
}

// nativeProbe records what the player asked the local proxy for and what the
// proxy answered, so a playback failure can be traced from the app log.
func (stream *nativeStreamServer) nativeProbe(format string, args ...any) {
	stream.probeMu.Lock()
	defer stream.probeMu.Unlock()
	stream.probes = append(stream.probes, fmt.Sprintf(format, args...))
	if len(stream.probes) > 64 {
		stream.probes = stream.probes[len(stream.probes)-64:]
	}
}

// nativeProbeLog returns the recorded proxy exchanges.
func (stream *nativeStreamServer) nativeProbeLog() []string {
	stream.probeMu.Lock()
	defer stream.probeMu.Unlock()
	return append([]string(nil), stream.probes...)
}

func (stream *nativeStreamServer) nativeRequest(request *http.Request) (*http.Response, error) {
	client := *stream.downloader.client
	client.Timeout = 0
	return stream.downloader.doMediaRequestWithClient(request, &client)
}

var nativePlaylistURI = regexp.MustCompile(`URI="([^"]+)"`)

func nativeHLSManifest(address string) bool {
	parsed, err := url.Parse(address)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(parsed.Path), ".m3u8")
}

// nativeDefiniteMedia reports whether a content type already identifies media
// that cannot be a playlist, so the body never needs to be sniffed first.
func nativeDefiniteMedia(contentType string) bool {
	value := strings.ToLower(strings.TrimSpace(contentType))
	if semicolon := strings.IndexByte(value, ';'); semicolon >= 0 {
		value = strings.TrimSpace(value[:semicolon])
	}
	if value == "" || value == "application/octet-stream" || strings.Contains(value, "mpegurl") {
		return false
	}
	return strings.HasPrefix(value, "video/") ||
		strings.HasPrefix(value, "audio/") ||
		value == "application/mp4"
}

const nativeStreamMaxAge = 10 * time.Minute

func newNativeStreamServer(d *Downloader) (*nativeStreamServer, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("无法初始化本机播放器")
	}
	stream := &nativeStreamServer{downloader: d, address: "http://" + listener.Addr().String(), sessions: map[string]*nativeStreamSession{}}
	server := &http.Server{Handler: http.HandlerFunc(stream.nativeServe), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	stream.server = server
	go func() { _ = server.Serve(listener) }()
	return stream, nil
}

func (stream *nativeStreamServer) nativeOpen(media providerMedia) (string, string) {
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(tokenBytes)
	ctx, cancel := context.WithCancel(providerMediaContext(context.Background(), media.credentials))
	// v3 fix: 把 seriesID/videoID 搬到 session，给 URL 过期时 refresh
	session := &nativeStreamSession{assets: map[string]nativeStreamAsset{}, referer: media.Referer, key: media.HLSKey, seriesID: media.seriesID, videoID: media.videoID, ctx: ctx, cancel: cancel, lastUsed: time.Now(), credentials: media.credentials}
	stream.mu.Lock()
	// 只按空闲时长回收。会话数量由 engine.playbacks 统一约束，并在那里调用
	// nativeRelease；这里再按数量淘汰会踢掉正在观看、恰好两次分片请求之间
	// 空闲的会话，播放器随后收到 410。
	for id, old := range stream.sessions {
		if time.Since(old.lastUsed) > nativeStreamMaxAge && !old.inFlight() {
			old.cancel()
			delete(stream.sessions, id)
		}
	}
	stream.sessions[token] = session
	stream.mu.Unlock()
	entry := nativeStreamAsset{address: media.URL, contentType: "video/mp4"}
	if media.Playlist != "" || len(media.HLSKey) > 0 || nativeHLSManifest(media.URL) {
		entry.contentType = "application/vnd.apple.mpegurl"
	}
	if media.Playlist != "" {
		entry.data = []byte(media.Playlist)
		entry.contentType = "application/vnd.apple.mpegurl"
	}
	return stream.nativeAsset(token, session, entry), token
}

func (stream *nativeStreamServer) nativeRelease(token string) {
	stream.mu.Lock()
	if session := stream.sessions[token]; session != nil {
		session.cancel()
		delete(stream.sessions, token)
	}
	stream.mu.Unlock()
}

func (stream *nativeStreamServer) nativeAsset(token string, session *nativeStreamSession, asset nativeStreamAsset) string {
	digest := sha256.Sum256([]byte(asset.address + "\x00" + asset.contentType))
	extension := ".ts"
	if parsed, err := url.Parse(asset.address); err == nil {
		switch candidate := strings.ToLower(path.Ext(parsed.Path)); candidate {
		case ".m3u8", ".ts", ".m4s", ".mp4", ".aac", ".m4a", ".mp3", ".vtt", ".webvtt", ".key":
			extension = candidate
		}
	}
	switch asset.contentType {
	case "application/vnd.apple.mpegurl":
		extension = ".m3u8"
	case "application/octet-stream":
		extension = ".key"
	case "video/mp4":
		extension = ".mp4"
	}
	id := hex.EncodeToString(digest[:12]) + extension
	session.mu.Lock()
	if previous, found := session.assets[id]; !found || len(previous.data) == 0 || len(asset.data) > 0 {
		session.assets[id] = asset
	}
	session.mu.Unlock()
	return stream.address + "/" + token + "/" + id
}

func (stream *nativeStreamServer) nativeRewrite(token string, session *nativeStreamSession, body, base string) (string, error) {
	var output []string
	nextPlaylist := false
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		text := strings.TrimSpace(line)
		rewrite := func(reference string, key bool, contentType string) string {
			if strings.HasPrefix(reference, "data:") {
				return reference
			}
			parsed, err := url.Parse(base)
			if err != nil {
				return ""
			}
			relative, err := url.Parse(reference)
			if err != nil {
				return ""
			}
			address := parsed.ResolveReference(relative).String()
			if !isProviderHTTPMediaURL(address) {
				return ""
			}
			asset := nativeStreamAsset{address: address, contentType: contentType}
			if key && len(session.key) == 16 {
				asset.data = append([]byte{}, session.key...)
				asset.contentType = "application/octet-stream"
			}
			return stream.nativeAsset(token, session, asset)
		}
		if text != "" && !strings.HasPrefix(text, "#") {
			contentType := ""
			if nextPlaylist {
				contentType = "application/vnd.apple.mpegurl"
			}
			line = rewrite(text, false, contentType)
			nextPlaylist = false
			if line == "" {
				return "", errors.New("播放列表中的媒体地址无效")
			}
		} else if strings.Contains(text, "URI=") {
			invalid := false
			line = nativePlaylistURI.ReplaceAllStringFunc(line, func(match string) string {
				reference := nativePlaylistURI.FindStringSubmatch(match)[1]
				key := strings.HasPrefix(text, "#EXT-X-KEY:") || strings.HasPrefix(text, "#EXT-X-SESSION-KEY:")
				contentType := ""
				switch {
				case key:
					contentType = "application/octet-stream"
				case strings.HasPrefix(text, "#EXT-X-MAP:"):
					contentType = "video/mp4"
				case strings.HasPrefix(text, "#EXT-X-MEDIA:"), strings.HasPrefix(text, "#EXT-X-I-FRAME-STREAM-INF:"), strings.HasPrefix(text, "#EXT-X-RENDITION-REPORT:"):
					contentType = "application/vnd.apple.mpegurl"
				}
				updated := rewrite(reference, key, contentType)
				if updated == "" {
					invalid = true
				}
				return "URI=\"" + updated + "\""
			})
			if invalid {
				return "", errors.New("播放列表中的附属地址无效")
			}
		}
		if strings.HasPrefix(text, "#EXT-X-STREAM-INF:") {
			nextPlaylist = true
		}
		output = append(output, line)
	}
	return strings.Join(output, "\n"), nil
}

// nativeRange 表示客户端请求的字节区间。suffix 为真时表示 "bytes=-N"（末尾 N 字节）。
type nativeRange struct {
	start  int64
	end    int64
	suffix bool
	open   bool
}

// nativeParseRange 解析单区间 Range 头。不支持的写法返回 ok=false，交由原样转发。
func nativeParseRange(value string) (nativeRange, bool) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return nativeRange{}, false
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(parts) != 2 {
		return nativeRange{}, false
	}
	start, end := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if start == "" {
		if end == "" {
			return nativeRange{}, false
		}
		size, err := strconv.ParseInt(end, 10, 64)
		if err != nil || size <= 0 {
			return nativeRange{}, false
		}
		return nativeRange{suffix: true, end: size}, true
	}
	from, err := strconv.ParseInt(start, 10, 64)
	if err != nil || from < 0 {
		return nativeRange{}, false
	}
	if end == "" {
		return nativeRange{start: from, open: true}, true
	}
	to, err := strconv.ParseInt(end, 10, 64)
	if err != nil || to < from {
		return nativeRange{}, false
	}
	return nativeRange{start: from, end: to}, true
}

// nativeSkip 丢弃上游响应开头的 n 字节。上游忽略 Range 并返回完整内容时，
// 代理必须自行跳过前缀，否则播放器拿到的是从 0 开始的错误偏移。
func nativeSkip(reader io.Reader, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := io.CopyN(io.Discard, reader, n)
	return err
}

func (stream *nativeStreamServer) nativeServe(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(writer, request)
		return
	}
	stream.mu.Lock()
	session := stream.sessions[parts[0]]
	if session != nil {
		session.lastUsed = time.Now()
	}
	stream.mu.Unlock()
	if session == nil {
		stream.nativeProbe("GET %s -> 410 会话不存在", parts[1])
		http.Error(writer, "播放已结束", http.StatusGone)
		return
	}
	stream.nativeProbe("GET %s range=%q", parts[1], request.Header.Get("Range"))
	session.mu.Lock()
	session.inflight++
	session.served++
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.inflight--
		session.lastUsed = time.Now()
		session.mu.Unlock()
	}()
	session.mu.Lock()
	asset, found := session.assets[parts[1]]
	session.mu.Unlock()
	if !found {
		http.NotFound(writer, request)
		return
	}
	ctx, cancel := context.WithCancel(providerMediaContext(request.Context(), session.credentials))
	defer cancel()
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	writer.Header().Set("Cache-Control", "no-store")
	if len(asset.data) > 0 && asset.total > int64(len(asset.data)) {
		if stream.nativeServePrefix(ctx, writer, request, session, asset) {
			return
		}
		asset.data = nil
	}
	if len(asset.data) > 0 {
		if strings.Contains(asset.contentType, "mpegurl") {
			body, err := stream.nativeRewrite(parts[0], session, string(asset.data), asset.address)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadGateway)
				return
			}
			writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			if request.Method == http.MethodGet {
				_, _ = io.WriteString(writer, body)
			}
		} else {
			writer.Header().Set("Content-Type", asset.contentType)
			if asset.etag != "" {
				writer.Header().Set("ETag", asset.etag)
			}
			http.ServeContent(writer, request, parts[1], time.Time{}, bytes.NewReader(asset.data))
		}
		return
	}
	upstream, err := http.NewRequestWithContext(ctx, request.Method, asset.address, nil)
	if err != nil {
		http.Error(writer, "媒体地址无效", http.StatusBadGateway)
		return
	}
	mediaRequestHeaders(upstream, session.referer)
	for _, name := range []string{"Range", "If-Range"} {
		if value := request.Header.Get(name); value != "" {
			upstream.Header.Set(name, value)
		}
	}
	response, err := stream.nativeRequest(upstream)
	if err != nil {
		http.Error(writer, "读取媒体失败，请重试", http.StatusBadGateway)
		return
	}
	// v3 fix: 红果 CDN URL 30 min 签名过期返回 403/410。拿到 403/410 后用 seriesID/videoID 重拉。
	if (response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusGone) &&
		session.seriesID != "" && session.videoID != "" && stream.downloader != nil {
		stream.downloader.recordDiagnostic(diagnosticEvent{Level: "info", Event: "media_refresh_triggered", Message: fmt.Sprintf("status=%d seriesID=%s videoID=%s old=%s", response.StatusCode, session.seriesID, session.videoID, asset.address)})
		response.Body.Close()
		response = nil
		newURL, refreshErr := stream.downloader.refreshHongguoMediaURL(ctx, session.seriesID, session.videoID)
		if refreshErr != nil {
			stream.downloader.recordDiagnostic(diagnosticEvent{Level: "warning", Event: "media_refresh_failed", Message: refreshErr.Error()})
		} else {
			stream.downloader.recordDiagnostic(diagnosticEvent{Level: "info", Event: "media_refresh_ok", Message: fmt.Sprintf("newURL=%s sameAsOld=%t", newURL, newURL == asset.address)})
		}
		if refreshErr == nil && newURL != "" && newURL != asset.address {
			asset.address = newURL
			upstream, err = http.NewRequestWithContext(ctx, request.Method, asset.address, nil)
			if err == nil {
				mediaRequestHeaders(upstream, session.referer)
				for _, name := range []string{"Range", "If-Range"} {
					if value := request.Header.Get(name); value != "" {
						upstream.Header.Set(name, value)
					}
				}
				response, err = stream.nativeRequest(upstream)
			}
		}
	}
	if err != nil {
		http.Error(writer, "读取媒体失败，请重试", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		err := stream.downloader.catalogResponseError(upstream, response, body)
		http.Error(writer, err.Error(), response.StatusCode)
		return
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	finalURL := upstream.URL
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL
	}
	// 上游可能忽略 Range 而返回 200 加完整内容（部分 CDN 如此）。此时必须由代理
	// 兑现客户端的区间请求：跳过前缀、限制长度并补上正确的 206 头，否则播放器
	// 会拿到从 0 开始的字节流，seek 之后解析错位并报 Source error。
	wanted, wantRange := nativeParseRange(request.Header.Get("Range"))
	rewriteRange := wantRange && response.StatusCode == http.StatusOK
	rangeStart, rangeEnd := int64(0), int64(-1)
	if rewriteRange {
		total := response.ContentLength
		if total < 0 {
			rewriteRange = false
		} else {
			switch {
			case wanted.suffix:
				if wanted.end > total {
					rangeStart = 0
				} else {
					rangeStart = total - wanted.end
				}
				rangeEnd = total - 1
			case wanted.open:
				rangeStart = wanted.start
				rangeEnd = total - 1
			default:
				rangeStart = wanted.start
				rangeEnd = min(wanted.end, total-1)
			}
			if rangeStart > rangeEnd || rangeStart >= total {
				writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
				writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
		}
	}
	playlist := strings.Contains(asset.contentType, "mpegurl") || strings.Contains(contentType, "mpegurl") || nativeHLSManifest(finalURL.String()) || len(session.key) > 0
	reader := bufio.NewReader(response.Body)
	if playlist {
		rewriteRange = false
	}
	if rewriteRange && request.Method == http.MethodGet {
		if err := nativeSkip(reader, rangeStart); err != nil {
			stream.nativeProbe("  -> 跳过前缀失败: %v", err)
			http.Error(writer, "读取媒体失败，请重试", http.StatusBadGateway)
			return
		}
	}
	// 仅在类型不确定时才探测首字节。Peek 会一直阻塞到上游送出数据为止，而播放器
	// 对响应头有连接超时（Media3 默认 8 秒）；上游稍慢就会让大文件开播失败。
	if !playlist && request.Method == http.MethodGet && !nativeDefiniteMedia(contentType) {
		peek, peekErr := reader.Peek(512)
		prefix := strings.TrimSpace(strings.TrimPrefix(string(peek), "\ufeff"))
		if strings.HasPrefix(prefix, "#EXTM3U") {
			playlist = true
		} else if peekErr == nil {
			playlist = false
		}
	}
	if playlist && request.Method == http.MethodHead {
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		writer.WriteHeader(http.StatusOK)
		return
	}
	if playlist && request.Method == http.MethodGet {
		body, err := io.ReadAll(io.LimitReader(reader, 4<<20+1))
		if err != nil || len(body) > 4<<20 {
			http.Error(writer, "播放列表过大或读取失败", http.StatusBadGateway)
			return
		}
		text := strings.TrimSpace(strings.TrimPrefix(string(body), "\ufeff"))
		if !strings.HasPrefix(text, "#EXTM3U") {
			http.Error(writer, "播放列表无效", http.StatusBadGateway)
			return
		}
		rewritten, err := stream.nativeRewrite(parts[0], session, text, finalURL.String())
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadGateway)
			return
		}
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(writer, rewritten)
		return
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if value := response.Header.Get(name); value != "" {
			writer.Header().Set(name, value)
		}
	}
	status := response.StatusCode
	if rewriteRange {
		status = http.StatusPartialContent
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.Header().Set("Content-Length", strconv.FormatInt(rangeEnd-rangeStart+1, 10))
		writer.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", rangeStart, rangeEnd, response.ContentLength))
	}
	writer.WriteHeader(status)
	// 立即把响应头送给播放器。否则头部会一直留在缓冲里，直到上游送出第一个
	// 字节才发出；上游稍慢就会触发播放器 8 秒连接超时（Media3 默认值）。
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	if request.Method == http.MethodGet {
		source := io.Reader(reader)
		if rewriteRange && rangeEnd >= rangeStart {
			source = io.LimitReader(reader, rangeEnd-rangeStart+1)
		}
		written, copyErr := io.Copy(writer, source)
		stream.nativeProbe("  -> %d bytes err=%v (upstream %d %q, range=%v)", written, copyErr,
			response.StatusCode, response.Header.Get("Content-Type"), rewriteRange)
	} else {
		stream.nativeProbe("  -> %d (head)", response.StatusCode)
	}
}
