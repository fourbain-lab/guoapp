package core

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveHongguoDanmaku(t *testing.T) {
	if os.Getenv("CHECK_LIVE_PROVIDERS") != "true" {
		t.Skip("set CHECK_LIVE_PROVIDERS=true to touch live provider text APIs")
	}
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.downloads.close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	catalog, err := engine.nativeCatalog(ctx, nativeInput{Action: "catalog", Source: sourceHongguo, Page: 1})
	if err != nil || len(catalog.Items) == 0 {
		t.Fatalf("catalog failed: count=%d err=%v", len(catalog.Items), err)
	}
	probed := 0
	for _, drama := range catalog.Items {
		if probed >= 5 {
			break
		}
		probed++
		detail, err := engine.nativeDetail(ctx, drama)
		if err != nil {
			t.Logf("%s detail failed: %v", drama.ID, err)
			continue
		}
		chapters, ok := detail.(map[string]any)["chapters"].([]Chapter)
		if !ok || len(chapters) == 0 {
			t.Logf("%s detail empty", drama.ID)
			continue
		}
		chapter := chapters[0]
		if !strings.HasPrefix(chapter.VideoURL, "hongguo-cenc://") {
			t.Logf("%s ep1 not cenc: %q", drama.ID, chapter.VideoURL)
			continue
		}
		plan, err := engine.nativeResolve(ctx, nativeInput{Action: "resolve", Drama: drama, Chapter: chapter, Index: 1})
		if err != nil {
			t.Logf("%s resolve failed: %v", drama.ID, err)
			continue
		}
		input := nativeInput{Action: "danmaku", PlaybackSession: plan.Session, StartMS: 0, DurationMS: 60_000}
		page, err := engine.nativeDanmaku(ctx, input)
		if err != nil {
			t.Errorf("%s danmaku FAILED: %v", drama.ID, err)
		} else {
			t.Logf("%s danmaku ok: items=%d total=%d next=%d", drama.ID, len(page.Items), page.Total, page.NextMS)
			for i, item := range page.Items {
				if i >= 3 {
					break
				}
				t.Logf("   id=%s t=%dms text=%q", item.ID, item.TimeMS, item.Text)
			}
		}
		engine.nativeReleasePlayback(plan.Session)
	}
}
