package adapter

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"
)

// streamIdleTimeout 上游 SSE 流无数据超时：超过该时长未收到任何数据视为流卡死，主动断开
const streamIdleTimeout = 180 * time.Second

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

	// 卡死检测：长时间无数据则关闭上游连接，中断阻塞的 Read，避免 goroutine/连接永久挂起
	idleTimer := time.AfterFunc(streamIdleTimeout, func() { resp.Body.Close() })
	defer idleTimer.Stop()

	reader := bufio.NewReader(resp.Body)

	for {
		line, err := reader.ReadSlice('\n')
		// ErrBufferFull 表示单行超过内部缓冲，改用 ReadBytes 读完整行
		if err == bufio.ErrBufferFull {
			line = append(append([]byte(nil), line...), readRemaining(reader)...)
			err = nil
		}
		if len(line) > 0 {
			// 收到数据即视为流存活，重置卡死计时
			idleTimer.Reset(streamIdleTimeout)
			// 直接写给客户端并立即刷盘；若客户端已断开，停止转发避免 goroutine 泄漏
			if _, werr := w.Write(line); werr != nil {
				return werr
			}
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

// readRemaining 读取到换行为止的剩余内容（配合 ReadSlice 的 ErrBufferFull 使用）
func readRemaining(reader *bufio.Reader) []byte {
	var tail []byte
	for {
		part, err := reader.ReadSlice('\n')
		tail = append(tail, part...)
		if err == nil || err == io.EOF {
			return tail
		}
		if err != bufio.ErrBufferFull {
			return tail
		}
	}
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
	// 逐行增量扫描前几行：每行到达立即检测并返回，避免 Peek 阻塞攒满缓冲导致首包延迟；
	// 扫描行数/字节数双上限，防止异常大行或长流拖慢透传（OpenCode 的错误总是流开头的第一条消息）
	const (
		maxScanBytes = 8 * 1024
		maxScanLines = 8
	)
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	var scanned []byte
	scanLines := 0

	for scanLines < maxScanLines && len(scanned) <= maxScanBytes {
		line, err := reader.ReadSlice('\n')
		// ErrBufferFull 表示单行超过内部缓冲，改用 ReadBytes 读完整行
		if err == bufio.ErrBufferFull {
			line = append(append([]byte(nil), line...), readRemaining(reader)...)
			err = nil
		}
		scanned = append(scanned, line...)
		scanLines++

		if CheckIsFreeUsageLimitError(scanned) {
			fullBody, _ := io.ReadAll(resp.Body)
			return append(scanned, fullBody...), true, nil
		}
		if err != nil {
			break
		}
	}

	// reader (bufio.Reader) 内部保留着未读的缓存数据，将其与已扫描的 scanned 拼接放回，
	// 保证后续透传完整无重复；切勿直接使用 MultiReader 拼接旧 body
	oldBody := resp.Body
	resp.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: io.MultiReader(bytes.NewReader(scanned), reader),
		Closer: oldBody,
	}

	return nil, false, nil
}
