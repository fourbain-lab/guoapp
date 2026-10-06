package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestZZ12IdleSessionEvictedByNewSessions reproduces the desync between the two
// separate session caches: the engine bounds engine.playbacks while the stream
// server independently bounds its own sessions. A player that pauses between
// range requests (the normal case) holds no in-flight request, so its session
// can be evicted underneath it and the next request answers 410 播放已结束.
func TestNativeIdlePlaybackSessionSurvivesOtherSessions(t *testing.T) {
	const total = 16 << 20
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"session-probe"`)
		w.Header().Set("Content-Length", strconv.Itoa(total))
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, &sessionProbeReader{total: total})
		}
	}))
	defer upstream.Close()

	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	open := func(index int) nativePlan {
		choice := nativePlaybackChoice{media: []providerMedia{{
			URL:     upstream.URL + fmt.Sprintf("/episode-%d.mp4", index),
			Referer: "https://example.test/watch",
		}}}
		plan, openErr := engine.nativeOpenPlayback(context.Background(), choice)
		if openErr != nil {
			t.Fatalf("open %d: %v", index, openErr)
		}
		return plan
	}

	// The user starts watching episode 0.
	playing := open(0)
	fetch := func() int {
		request, _ := http.NewRequest(http.MethodGet, playing.URL, nil)
		request.Header.Set("Range", "bytes=0-65535")
		response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
		if err != nil {
			return -1
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		return response.StatusCode
	}
	if code := fetch(); code != http.StatusOK && code != http.StatusPartialContent {
		t.Fatalf("[session] cannot start playback: %d", code)
	}

	// Playback pauses (buffering, user pausing, or simply between range reads)
	// while the app opens other episodes: retries, preloading, browsing.
	for i := 1; i <= 12; i++ {
		open(i)
	}

	code := fetch()
	fmt.Fprintf(os.Stderr, "[session] resuming the original episode -> %d\n", code)
	if code == http.StatusGone {
		t.Errorf("[session] the episode being watched was evicted while idle: %d (播放已结束)", code)
	}
	if code != http.StatusOK && code != http.StatusPartialContent {
		t.Errorf("[session] unexpected status %d", code)
	}
}

type sessionProbeReader struct {
	total  int
	offset int
}

func (r *sessionProbeReader) Read(p []byte) (int, error) {
	if r.offset >= r.total {
		return 0, io.EOF
	}
	n := len(p)
	if n > 128<<10 {
		n = 128 << 10
	}
	if n > r.total-r.offset {
		n = r.total - r.offset
	}
	for i := 0; i < n; i++ {
		p[i] = byte((r.offset + i) % 251)
	}
	r.offset += n
	return n, nil
}
