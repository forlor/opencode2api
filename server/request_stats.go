package server

import (
	"fmt"
	"log"
	"strings"
)

// reqStats 单个请求的体积统计，用于排查"会话越用越大 / Request too large"类问题
type reqStats struct {
	msgs     int   // messages/contents 条数
	imgs     int   // 内联 base64 图片数
	imgBytes int64 // 内联图片 base64 数据总字节数
	urlImgs  int   // URL 引用图片数（入站不占体积，Gemini 线路会下载转 base64 导致出站膨胀）
}

const mbSize = 1024 * 1024

// logRequestStats 在入口解析成功后输出一行请求体积统计。
// 排查方法：客户端报 "Request too large (max 32MB)" 时——
//   - 日志中无对应行 → 客户端在发送前本地拦截，请求未到达代理；
//   - 有对应行且 body 接近上限 → 同时会出现"读请求体失败"日志，说明被代理 10MB 入站上限挡下。
func logRequestStats(path string, payload map[string]any, bodyLen int) {
	st := collectRequestStats(payload)

	model, _ := payload["model"].(string)
	stream := ""
	if v, ok := payload["stream"].(bool); ok {
		stream = fmt.Sprintf(" stream=%v", v)
	}
	warn := ""
	if bodyLen >= 8*mbSize {
		warn = " (接近10MB入站上限)"
	}
	log.Printf("[req-stats] path=%s model=%s%s body=%.2fMB msgs=%d imgs=%d(imgBytes=%.2fMB) urlImgs=%d%s",
		path, model, stream,
		float64(bodyLen)/mbSize, st.msgs, st.imgs, float64(st.imgBytes)/mbSize, st.urlImgs, warn)
}

// logOutboundStats 出站体积对比日志：观察协议转换 / URL 图内联导致的请求膨胀
func logOutboundStats(path, model string, inbound, outbound int) {
	log.Printf("[req-stats] 出站 path=%s model=%s 入站=%.2fMB 转换后出站=%.2fMB",
		path, model, float64(inbound)/mbSize, float64(outbound)/mbSize)
}

// collectRequestStats 解析 payload 并统计消息数与图片占用，兼容三种入站协议的图片格式
func collectRequestStats(payload map[string]any) reqStats {
	st := reqStats{}
	if msgs, ok := payload["messages"].([]any); ok {
		st.msgs = len(msgs)
	}
	if contents, ok := payload["contents"].([]any); ok {
		st.msgs = len(contents)
	}
	walkImageStats(payload, &st)
	return st
}

// walkImageStats 递归识别各协议图片块：
//   - Anthropic:  {type:"image", source:{type:"base64"|"url", ...}}
//   - OpenAI chat: {type:"image_url", image_url:{url:...}}
//   - Responses:  {type:"input_image", image_url:...}
//   - Gemini 原生: {inlineData:{data:...}} / {inline_data:{data:...}}
//
// 命中图片块后不再下钻其子节点，避免重复计数。
func walkImageStats(v any, st *reqStats) {
	switch t := v.(type) {
	case map[string]any:
		typ, _ := t["type"].(string)
		if typ == "image" {
			if src, ok := t["source"].(map[string]any); ok {
				switch src["type"] {
				case "base64":
					if d, _ := src["data"].(string); d != "" {
						st.imgs++
						st.imgBytes += int64(len(d))
					}
				case "url":
					st.urlImgs++
				}
			}
			return
		}
		if typ == "input_image" {
			countImageRef(t["image_url"], st)
			return
		}
		if iu, ok := t["image_url"]; ok {
			countImageRef(iu, st)
			return
		}
		for _, key := range []string{"inlineData", "inline_data"} {
			if inl, ok := t[key].(map[string]any); ok {
				if d, _ := inl["data"].(string); d != "" {
					st.imgs++
					st.imgBytes += int64(len(d))
				}
				return
			}
		}
		for _, child := range t {
			walkImageStats(child, st)
		}
	case []any:
		for _, child := range t {
			walkImageStats(child, st)
		}
	}
}

// countImageRef OpenAI 风格图片引用：字符串或 {url:...} 对象两种形态
func countImageRef(v any, st *reqStats) {
	switch u := v.(type) {
	case string:
		countImageURL(u, st)
	case map[string]any:
		if s, ok := u["url"].(string); ok {
			countImageURL(s, st)
		}
	}
}

// countImageURL data URI 计入内联体积（取逗号后的 base64 部分），http(s) URL 计为 urlImg
func countImageURL(u string, st *reqStats) {
	switch {
	case strings.HasPrefix(u, "data:"):
		if i := strings.Index(u, ","); i >= 0 {
			st.imgs++
			st.imgBytes += int64(len(u) - i - 1)
		}
	case strings.HasPrefix(u, "http://"), strings.HasPrefix(u, "https://"):
		st.urlImgs++
	}
}
