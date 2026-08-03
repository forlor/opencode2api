package adapter

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
)

// HandleStreamForwarding 读取 OpenCode 的 SSE 响应，将其实时透传给客户端，并支持检测流初期的错误
func HandleStreamForwarding(w http.ResponseWriter, resp *http.Response) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("http.ResponseWriter 不支持 Streaming Flush")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	reader := bufio.NewReader(resp.Body)

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			// 直接写给客户端并立即刷盘
			w.Write(line)
			flusher.Flush()
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	return nil
}

// IsTemporaryServerError 判断是否为 503/502/500 等 OpenCode 服务端临时抖动错误
func IsTemporaryServerError(statusCode int) bool {
	return statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusBadGateway ||
		statusCode == http.StatusInternalServerError ||
		statusCode == http.StatusGatewayTimeout
}

// ReadAndCheckStreamInitialError 读取流的开头几字节/几行，判断是否立即抛出了 FreeUsageLimitError 错误
// 如果没有错误，返回读出的预读字节和 false，如果有 429 错误返回完整的 body 和 true
func ReadAndCheckStreamInitialError(resp *http.Response) ([]byte, bool, error) {
	// 如果 HTTP 状态码不是 200，说明直接返回了 JSON 报错
	if resp.StatusCode != http.StatusOK {
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, false, err
		}
		if CheckIsFreeUsageLimitError(bodyBytes) || resp.StatusCode == http.StatusTooManyRequests {
			return bodyBytes, true, nil
		}
		return bodyBytes, false, nil
	}

	// 状态码是 200 时，仍有可能是包裹在 SSE 数据中的错误 JSON
	reader := bufio.NewReader(resp.Body)
	peekBytes, err := reader.Peek(512)
	if err != nil && err != io.EOF {
		return nil, false, err
	}

	if CheckIsFreeUsageLimitError(peekBytes) {
		fullBody, _ := io.ReadAll(resp.Body)
		return append(peekBytes, fullBody...), true, nil
	}

	// reader (bufio.Reader) 内部已保留 Peek 过的缓存数据，直接使用 reader 即可，切勿使用 MultiReader 重复拼接
	oldBody := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: reader,
		Closer: oldBody,
	}

	return nil, false, nil
}
