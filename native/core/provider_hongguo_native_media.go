package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (downloader *Downloader) resolveHongguoAppMedia(ctx context.Context, videoID string) (providerMedia, error) {
	if !hongguoNumericID.MatchString(videoID) {
		return providerMedia{}, errors.New("红果视频 ID 无效")
	}
	payload := map[string]any{
		"video_id": videoID, "content_type": 1,
		"biz_param": map[string]any{"need_all_video_definition": true, "video_platform": 3},
	}
	result, err := downloader.hongguoAppRequest(ctx, http.MethodPost, "/novel/player/video_model/v1/", nil, payload)
	if err != nil {
		return providerMedia{}, err
	}
	data := nestedMap(result, "data")
	model, _ := data["video_model"].(map[string]any)
	if encoded, ok := data["video_model"].(string); ok {
		decoder := json.NewDecoder(strings.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&model); err != nil {
			return providerMedia{}, errors.New("红果 App 播放信息格式异常")
		}
	}
	return selectHongguoAppMedia(model, videoID, videoID)
}

func selectHongguoAppMedia(model map[string]any, seriesID, videoID string) (providerMedia, error) {
	variants := anyList(model["video_list"])
	if rows, ok := model["video_list"].(map[string]any); ok && len(variants) == 0 {
		keys := make([]string, 0, len(rows))
		for key := range rows {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			variants = append(variants, rows[key])
		}
	}
	duration, _ := strconv.ParseFloat(mapString(model, "video_duration", "duration"), 64)
	var keyErr error
	type scoredMedia struct {
		media providerMedia
		score int
	}
	var choices []scoredMedia
	for _, row := range variants {
		variant, _ := row.(map[string]any)
		meta := nestedMap(variant, "video_meta")
		codec := strings.ToLower(mapString(meta, "codec_type"))
		// v8 fix: 只过滤 codec_type=="bytevc2"，不要再用 gear_des_key 含 bytevc2 判断
		// 否则会误伤像 1080p h265_hvc1 这种 variant (gear_des_key 链里含 bytevc2 字面量)
		// v10 fix: bytevc1 也过滤 (MediaCodec 都不解)，作为 defense-in-depth 防 reorder 回归
		// v11 fix (2026-10-07): 仿 yt-dlp #9575 — bytevc 不直接 ban，降权到 -10000
		//   原因：fork 部署在 Intel Mac 上无安卓 codec，但 fork 也跑在用户手机/树莓派等 Android 端
		//   用户设备上的 MediaCodec 也许能解（部分版本 bytevc1 = h265 重编码），降权保留最后尝试
		//   h264 score = height*10 + 1 (max ~1921) → bytevc score -= 10000 保证永远垫底
		//   关键诊断文件：/tmp/guoapp_v2/BYTEVC_DIAGNOSIS.md (双剧8集实测，web 前3集后4集起SSR 404)
		isBytevc := codec == "bytevc2" || codec == "bytevc1"
		addresses := hongguoMediaAddresses(variant)
		if len(addresses) == 0 {
			continue
		}
		// v2 fix: 不再写死 novel.snssdk.com，留空 Referer，让 mediaRequestHeaders 智能兜底
		// v3 fix: seriesID/videoID/source 留底，给 30min 签名过期时 refreshHongguoMediaURL 重拉
		// v9 fix: Referer 留空（curl 实测无 Referer = 206，写 hongguoduanju.com = 403 denied by Referer ACL）
		media := providerMedia{Referer: "", Duration: time.Duration(duration * float64(time.Second)), seriesID: seriesID, videoID: videoID, source: sourceHongguo}
		encryption := nestedMap(variant, "encrypt_info")
		spade := mapString(encryption, "spade_a")
		if spade != "" || encryption["encrypt"] == true || mapString(encryption, "encryption_method") == "cenc-aes-ctr" {
			var err error
			media.CENCKey, err = hongguoContentKey(spade)
			if err != nil {
				keyErr = err
				continue
			}
		}
		height, _ := strconv.Atoi(mapString(meta, "vheight"))
		if definition, err := strconv.Atoi(hongguoQualityNumber.FindString(mapString(meta, "definition"))); err == nil && definition > 0 {
			height = definition
		} else if width, _ := strconv.Atoi(mapString(meta, "vwidth")); width > 0 && (height == 0 || width < height) {
			height = width
		}
		media.Quality = height
		quality := height * 10
		if codec == "h264" || codec == "avc1" {
			quality++
		}
		// v11 fix: bytevc 降权（h264 最高 ~43201 8K, bytevc 仅作 last-resort）
		//   Quality=0 让 nativePlaybackChoices 重新按 Quality 排序时 bytevc 也排最后
		//   score 也降权 -100000 让 selectHongguoAppMedia 内部选择 score 最高时也排最后
		if isBytevc {
			media.Quality = 0
			quality -= 100000
		}
		for _, address := range addresses {
			media.URL = address
			choices = append(choices, scoredMedia{media: media, score: quality})
		}
	}
	if len(choices) > 0 {
		sort.SliceStable(choices, func(i, j int) bool { return choices[i].score > choices[j].score })
		selected := choices[0].media
		for _, choice := range choices {
			selected.Variants = append(selected.Variants, choice.media)
		}
		return selected, nil
	}
	if keyErr != nil {
		return providerMedia{}, fmt.Errorf("红果 App 媒体密钥不可用: %w", keyErr)
	}
	return providerMedia{}, errors.New("红果 App 仅返回 bytevc1/bytevc2 (fork 不解码)，请等待服务端恢复或装正版 App")
}

func hongguoMediaAddresses(info map[string]any) []string {
	var addresses []string
	seen := map[string]bool{}
	var add func(any)
	add = func(value any) {
		switch value := value.(type) {
		case string:
			address := strings.TrimSpace(value)
			if len(address) > 8192 {
				return
			}
			if !isProviderHTTPMediaURL(address) {
				decoded, err := decodeHongguoBase64(address)
				if err != nil {
					return
				}
				address = strings.TrimSpace(string(decoded))
			}
			if isProviderHTTPMediaURL(address) && !seen[address] {
				seen[address] = true
				addresses = append(addresses, address)
			}
		case []any:
			for _, item := range value {
				add(item)
			}
		}
	}
	for _, key := range []string{"main_url", "backup_url", "backup_url_1", "backup_url_2", "backup_urls", "url_list"} {
		add(info[key])
	}
	return addresses
}
