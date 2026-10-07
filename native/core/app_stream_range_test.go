package core

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

// rangeProbeReader emits a payload where byte i equals i%251, so a wrong offset is
// immediately detectable.
type rangeProbeReader struct {
	total  int
	offset int
}

func (r *rangeProbeReader) Read(p []byte) (int, error) {
	if r.offset >= r.total {
		return 0, io.EOF
	}
	n := len(p)
	if n > 256<<10 {
		n = 256 << 10
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

// TestZZ14UpstreamIgnoresRange reproduces the real CDN behaviour observed in the
// device log: a Range request is answered with 200 and the whole file. The proxy
// must still honour the requested offset, otherwise a seek hands the player
// mismatched bytes and it reports a source error.
func TestNativeUpstreamIgnoringRangeStillHonoursOffsets(t *testing.T) {
	const total = 5 << 20
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately ignore Range: always 200 with the entire body.
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, &rangeProbeReader{total: total})
		}
	}))
	defer upstream.Close()

	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stream, err := newNativeStreamServer(engine.downloader)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.server.Close()
	address, token := stream.nativeOpen(providerMedia{
		URL:     upstream.URL + "/episode.mp4",
		Referer: "https://example.test/watch",
	})
	defer stream.nativeRelease(token)

	client := &http.Client{Timeout: 60 * time.Second}
	type probe struct {
		rangeHeader string
		start       int64
		length      int64
	}
	probes := []probe{
		{"bytes=0-", 0, total},
		{"bytes=1048576-", 1048576, total - 1048576},
		{"bytes=124194-", 124194, total - 124194},
		{"bytes=1000-1999", 1000, 1000},
		{fmt.Sprintf("bytes=-%d", 4096), total - 4096, 4096},
	}
	for _, p := range probes {
		request, _ := http.NewRequest(http.MethodGet, address, nil)
		request.Header.Set("Range", p.rangeHeader)
		start := time.Now()
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("[range] %s: %v", p.rangeHeader, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		response.Body.Close()
		fmt.Fprintf(os.Stderr, "[range] %-20s -> status=%d len=%q range=%q got=%d err=%v in %v\n",
			p.rangeHeader, response.StatusCode, response.Header.Get("Content-Length"),
			response.Header.Get("Content-Range"), len(body), readErr, time.Since(start))

		if response.StatusCode != http.StatusPartialContent {
			t.Errorf("[range] %s: expected 206, got %d (player asked for an offset but got the whole file)",
				p.rangeHeader, response.StatusCode)
		}
		if len(body) > 0 {
			if want := byte(p.start % 251); body[0] != want {
				t.Errorf("[range] %s: first byte %d, want %d (wrong offset delivered)",
					p.rangeHeader, body[0], want)
			}
		}
		if int64(len(body)) != p.length {
			t.Errorf("[range] %s: got %d bytes, want %d", p.rangeHeader, len(body), p.length)
		}
		if want := fmt.Sprintf("bytes %d-%d/%d", p.start, p.start+p.length-1, total); response.Header.Get("Content-Range") != want {
			t.Errorf("[range] %s: Content-Range %q, want %q",
				p.rangeHeader, response.Header.Get("Content-Range"), want)
		}
	}
}
