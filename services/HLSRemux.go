package services

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// remuxRemoteHLS uses FFmpeg only for container/playlist processing: -c copy
// avoids video/audio re-encoding. FFmpeg's HTTP requests are routed through a
// loopback-only proxy so every playlist, segment, key, and redirect continues
// to use the same public-address checks as ordinary remote downloads.
func remuxRemoteHLS(ctx context.Context, sourceURL string, initialPlaylist []byte, initialContentType, outputPath string, maxOutputBytes int64) error {
	rewrittenPlaylist, err := rewriteHLSPlaylistURLs(sourceURL, initialPlaylist)
	if err != nil {
		return fmt.Errorf("rewrite HLS playlist URLs: %w", err)
	}
	proxyURL, initialPlaylistURL, closeProxy, err := startSafeHLSProxy(rewrittenPlaylist, initialContentType)
	if err != nil {
		return fmt.Errorf("start safe HLS proxy: %w", err)
	}
	defer closeProxy()

	args := []string{
		"-hide_banner", "-y", "-loglevel", "error",
		"-http_proxy", proxyURL,
		"-protocol_whitelist", "http,https,httpproxy,tcp,tls,crypto",
		"-allowed_extensions", "ALL",
		"-i", initialPlaylistURL,
		"-map", "0:v:0", "-map", "0:a?",
		"-c", "copy", "-movflags", "+faststart",
		"-f", "mp4", outputPath,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr cappedBuffer
	stderr.limit = 64 * 1024
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-wait:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				detail := sanitizeFFmpegError(stderr.String())
				if detail == "" {
					return fmt.Errorf("ffmpeg HLS-to-MP4 remux failed: %w", err)
				}
				return fmt.Errorf("ffmpeg HLS-to-MP4 remux failed: %w: %s", err, detail)
			}
			info, statErr := os.Stat(outputPath)
			if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
				return fmt.Errorf("ffmpeg completed without a valid MP4 output")
			}
			if maxOutputBytes > 0 && info.Size() > maxOutputBytes {
				return fmt.Errorf("remuxed video exceeds maximum file size (%d bytes)", maxOutputBytes)
			}
			return nil
		case <-ctx.Done():
			// CommandContext terminates FFmpeg. Wait for the process to exit before
			// returning so callers can safely remove its partial output.
			<-wait
			return ctx.Err()
		case <-ticker.C:
			if maxOutputBytes <= 0 {
				continue
			}
			info, statErr := os.Stat(outputPath)
			if statErr == nil && info.Size() > maxOutputBytes {
				_ = cmd.Process.Kill()
				<-wait
				return fmt.Errorf("remuxed video exceeds maximum file size (%d bytes)", maxOutputBytes)
			}
		}
	}
}

func startSafeHLSProxy(initialPlaylist []byte, initialContentType string) (proxyURL, initialPlaylistURL string, closeProxy func(), err error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", nil, err
	}
	server := &http.Server{Handler: safeHLSProxyHandler{
		initialPlaylistPath: "/__videocms_initial_playlist__.m3u8",
		initialPlaylist:     initialPlaylist,
		initialContentType:  initialContentType,
	}}
	go func() {
		_ = server.Serve(listener)
	}()
	proxyURL = "http://" + listener.Addr().String()
	initialPlaylistURL = proxyURL + "/__videocms_initial_playlist__.m3u8"
	return proxyURL, initialPlaylistURL, func() {
		_ = server.Close()
		_ = listener.Close()
	}, nil
}

type safeHLSProxyHandler struct {
	initialPlaylistPath string
	initialPlaylist     []byte
	initialContentType  string
}

func (h safeHLSProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Serve the already-fetched root playlist from loopback. Relative playlist
	// references are rewritten to absolute source URLs, so every subsequent
	// request still passes through the public-address-checking proxy.
	if r.URL != nil && r.URL.Path == h.initialPlaylistPath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if h.initialContentType != "" {
			w.Header().Set("Content-Type", h.initialContentType)
		} else {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(h.initialPlaylist)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(h.initialPlaylist)
		}
		return
	}
	if r.Method == http.MethodConnect {
		handleSafeHLSConnect(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "proxy method is not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL == nil || !r.URL.IsAbs() || (r.URL.Scheme != "http" && r.URL.Scheme != "https") || r.URL.Host == "" {
		http.Error(w, "proxy requires an absolute HTTP(S) URL", http.StatusBadRequest)
		return
	}

	request, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, "invalid proxied request", http.StatusBadRequest)
		return
	}
	request.Header = r.Header.Clone()
	request.Host = r.URL.Host
	for name := range request.Header {
		if strings.HasPrefix(strings.ToLower(name), "proxy-") {
			request.Header.Del(name)
		}
	}
	for name := range request.Header {
		if isHopByHopProxyHeader(name) {
			request.Header.Del(name)
		}
	}

	response, err := secureHTTPClient().Do(request)
	if err != nil {
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		if isHopByHopProxyHeader(name) {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, response.Body)
	}
}

func handleSafeHLSConnect(w http.ResponseWriter, r *http.Request) {
	address := strings.TrimSpace(r.Host)
	if address == "" {
		http.Error(w, "missing CONNECT host", http.StatusBadRequest)
		return
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(strings.Trim(address, "[]"), "443")
	}
	upstream, err := dialPublicAddress(r.Context(), "tcp", address)
	if err != nil {
		http.Error(w, "CONNECT target is not allowed", http.StatusForbidden)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "CONNECT is not supported", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	if err := buffered.Flush(); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}

	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, buffered.Reader)
		if closeWriter, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		if closeWriter, ok := client.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
		copyDone <- struct{}{}
	}()
	<-copyDone
	<-copyDone
	_ = client.Close()
	_ = upstream.Close()
}

func isHopByHopProxyHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(name), "proxy-")
	}
}

var hlsURIAttributePattern = regexp.MustCompile(`(?i)(URI\s*=\s*")([^"]+)(")`)

// rewriteHLSPlaylistURLs makes relative segment, variant, map, and key URIs
// absolute against the original playlist URL before serving it on loopback.
func rewriteHLSPlaylistURLs(baseURL string, playlist []byte) ([]byte, error) {
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("invalid HLS base URL")
	}
	lines := strings.Split(string(playlist), "\n")
	for i, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			lines[i] = hlsURIAttributePattern.ReplaceAllStringFunc(rawLine, func(match string) string {
				parts := hlsURIAttributePattern.FindStringSubmatch(match)
				if len(parts) != 4 {
					return match
				}
				resolved, ok := resolveHLSReference(base, parts[2])
				if !ok {
					return match
				}
				return parts[1] + resolved + parts[3]
			})
			continue
		}
		resolved, ok := resolveHLSReference(base, line)
		if ok {
			lines[i] = resolved
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func resolveHLSReference(base *url.URL, reference string) (string, bool) {
	referenceURL, err := url.Parse(strings.TrimSpace(reference))
	if err != nil {
		return "", false
	}
	if referenceURL.Scheme != "" && referenceURL.Scheme != "http" && referenceURL.Scheme != "https" {
		return "", false
	}
	resolved := base.ResolveReference(referenceURL)
	if (resolved.Scheme != "http" && resolved.Scheme != "https") || resolved.Host == "" {
		return "", false
	}
	return resolved.String(), true
}

// cappedBuffer bounds FFmpeg diagnostics so a malformed playlist cannot create
// an unbounded error string. It still reports all input bytes as consumed.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
		} else {
			_, _ = b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

var remoteURLInDiagnosticPattern = regexp.MustCompile(`(?i)https?://[^\s\"'<>]+`)

func sanitizeFFmpegError(output string) string {
	return strings.TrimSpace(remoteURLInDiagnosticPattern.ReplaceAllString(output, "[remote URL]"))
}
