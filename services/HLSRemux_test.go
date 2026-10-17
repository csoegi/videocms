package services

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestIsHLSPlaylistResponseByURLAndContentType(t *testing.T) {
	response := &http.Response{Request: &http.Request{URL: mustParseURL(t, "https://media.example/video/master.m3u8?token=test")}}
	if !isHLSPlaylistResponse("https://media.example/video/master.m3u8?token=test", response) {
		t.Fatal("expected .m3u8 URL to be recognized as HLS")
	}

	response = &http.Response{Header: http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl; charset=utf-8"}}}
	if !isHLSPlaylistResponse("https://media.example/video/master", response) {
		t.Fatal("expected HLS content type to be recognized")
	}

	response = &http.Response{Header: http.Header{"Content-Type": []string{"video/mp4"}}}
	if isHLSPlaylistResponse("https://media.example/video/movie.mp4", response) {
		t.Fatal("MP4 response should not be recognized as HLS")
	}
}

func TestHLSMP4OutputName(t *testing.T) {
	cases := []struct {
		input string
		id    uint
		want  string
	}{
		{input: "master.m3u8", id: 3, want: "master.mp4"},
		{input: "movie.m3u8", id: 4, want: "movie.mp4"},
		{input: "", id: 5, want: "remote-download-5.mp4"},
	}
	for _, tc := range cases {
		if got := hlsMP4OutputName(tc.input, tc.id); got != tc.want {
			t.Errorf("hlsMP4OutputName(%q, %d) = %q; want %q", tc.input, tc.id, got, tc.want)
		}
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestSafeHLSProxyServesCachedRootPlaylist(t *testing.T) {
	playlist := "#EXTM3U\n#EXT-X-ENDLIST\n"
	handler := safeHLSProxyHandler{
		initialPlaylistPath: "/__videocms_initial_playlist__.m3u8",
		initialPlaylist:     []byte(playlist),
		initialContentType:  "application/vnd.apple.mpegurl",
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:12345/__videocms_initial_playlist__.m3u8", nil)
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Content-Length"); got != strconv.Itoa(len(playlist)) {
		t.Fatalf("Content-Length = %q, want %d", got, len(playlist))
	}
	if got := recorder.Body.String(); got != playlist {
		t.Fatalf("cached body = %q, want %q", got, playlist)
	}
}

func TestRewriteHLSPlaylistURLs(t *testing.T) {
	input := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"keys/key.bin?token=abc\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nvariants/720p.m3u8\n"
	got, err := rewriteHLSPlaylistURLs("https://media.example/videos/master.m3u8?sig=root", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	want := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"https://media.example/videos/keys/key.bin?token=abc\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nhttps://media.example/videos/variants/720p.m3u8\n"
	if string(got) != want {
		t.Fatalf("rewritten playlist = %q, want %q", string(got), want)
	}
}

func TestSanitizeFFmpegErrorRedactsRemoteURLs(t *testing.T) {
	got := sanitizeFFmpegError("failed to open https://media.example/path/secret?token=abc")
	if strings.Contains(got, "token=abc") || strings.Contains(got, "secret") {
		t.Fatalf("remote URL was not redacted: %q", got)
	}
	if !strings.Contains(got, "[remote URL]") {
		t.Fatalf("redacted error = %q", got)
	}
}
