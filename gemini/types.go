package gemini

import "net/http"

// Gemini 请求/响应数据结构。
// 请求侧用"类型化 + 动态字段"混合：顶层/常用字段强类型，functionCall.args、
// functionResponse.response、JSON Schema（parameters/responseSchema）用 any 走程序化转换。

// DummyThoughtSignature Gemini 3 对 functionCall 的校验占位签名（仅在每条首 functionCall 上附加）。
// 行为可能随 API 版本变化；收敛在本常量，便于单点修改。
const DummyThoughtSignature = "context_engineering_is_the_way_to_go"

// ThinkingLevelMap 模型后缀 thinkingLevel（小写）→ Gemini API 值（大写）
var ThinkingLevelMap = map[string]string{
	"high":    "HIGH",
	"low":     "LOW",
	"medium":  "MEDIUM",
	"minimal": "MINIMAL",
}

// GeminiRequest 发往 generateContent / streamGenerateContent 的请求体
type GeminiRequest struct {
	Contents          []GeminiContent        `json:"contents"`
	SystemInstruction *GeminiContent         `json:"systemInstruction,omitempty"`
	Tools             []GeminiTool           `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig      `json:"toolConfig,omitempty"`
	GenerationConfig  *GeminiGenerationConfig `json:"generationConfig,omitempty"`
	SafetySettings    []GeminiSafetySetting  `json:"safetySettings,omitempty"`
}

// GeminiContent 一段对话内容（role: user / model / system）
type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts"`
}

// GeminiPart 一个 part，通过"多个指针字段、最多一个非空"实现"只有一个生效"的语义
type GeminiPart struct {
	Text             *string                 `json:"text,omitempty"`
	Thought          *bool                   `json:"thought,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
	InlineData       *GeminiInlineData       `json:"inlineData,omitempty"`
	FunctionCall     *GeminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
}

// GeminiInlineData base64 图片等内联数据
type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// GeminiFunctionCall 模型主动调用的函数
type GeminiFunctionCall struct {
	Name string `json:"name"`
	Args any    `json:"args,omitempty"`
}

// GeminiFunctionResponse 工具执行结果；Response 必须是 object（Struct），不能是数组/原始值
type GeminiFunctionResponse struct {
	Name     string `json:"name"`
	Response any    `json:"response"`
}

// GeminiTool 一个工具：functionDeclarations 或内置工具（googleSearch/codeExecution/urlContext 等）
type GeminiTool struct {
	FunctionDeclarations  []GeminiFunctionDeclaration `json:"functionDeclarations,omitempty"`
	GoogleSearch          any                         `json:"googleSearch,omitempty"`
	GoogleSearchRetrieval any                         `json:"googleSearchRetrieval,omitempty"`
	CodeExecution         any                         `json:"codeExecution,omitempty"`
	URLContext            any                         `json:"urlContext,omitempty"`
}

// GeminiFunctionDeclaration 函数声明；Parameters 是转换后的 JSON Schema（any）
type GeminiFunctionDeclaration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// GeminiToolConfig 工具调用配置
type GeminiToolConfig struct {
	FunctionCallingConfig           *GeminiFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
	IncludeServerSideToolInvocations *bool                       `json:"includeServerSideToolInvocations,omitempty"`
}

// GeminiFunctionCallingConfig 工具选择模式
type GeminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"` // AUTO|ANY|NONE
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// GeminiGenerationConfig 采样与输出配置
type GeminiGenerationConfig struct {
	MaxOutputTokens  int       `json:"maxOutputTokens,omitempty"`
	StopSequences    []string  `json:"stopSequences,omitempty"`
	Temperature      *float64  `json:"temperature,omitempty"`
	TopK             *float64  `json:"topK,omitempty"`
	TopP             *float64  `json:"topP,omitempty"`
	ResponseMimeType string    `json:"responseMimeType,omitempty"`
	ResponseSchema   any       `json:"responseSchema,omitempty"`
	ThinkingConfig   *GeminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

// GeminiThinkingConfig 思考配置（Gemini 2.5+/3）
type GeminiThinkingConfig struct {
	IncludeThoughts *bool  `json:"includeThoughts,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"` // HIGH|LOW|MEDIUM|MINIMAL
}

// GeminiSafetySetting 安全设置
type GeminiSafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

// ==================== 响应侧 ====================

// GeminiResponse generateContent / 流式 chunk / 错误体的统一结构
type GeminiResponse struct {
	Candidates    []GeminiCandidate    `json:"candidates,omitempty"`
	UsageMetadata *GeminiUsageMetadata `json:"usageMetadata,omitempty"`
	PromptFeedback *GeminiPromptFeedback `json:"promptFeedback,omitempty"`
}

// GeminiCandidate 一个候选回答
type GeminiCandidate struct {
	Content      *GeminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"` // STOP|MAX_TOKENS|SAFETY|RECITATION|OTHER
	Index        int            `json:"index,omitempty"`
}

// GeminiUsageMetadata token 用量
type GeminiUsageMetadata struct {
	PromptTokenCount        int64               `json:"promptTokenCount"`
	CandidatesTokenCount    int64               `json:"candidatesTokenCount"`
	TotalTokenCount         int64               `json:"totalTokenCount"`
	ThoughtsTokenCount      int64               `json:"thoughtsTokenCount,omitempty"`
	ToolUsePromptTokenCount int64               `json:"toolUsePromptTokenCount,omitempty"`
	CandidatesTokensDetails []GeminiTokensDetail `json:"candidatesTokensDetails,omitempty"`
}

// GeminiTokensDetail 按 modality 拆分的 token 明细
type GeminiTokensDetail struct {
	Modality   string `json:"modality"`
	TokenCount int64  `json:"tokenCount"`
}

// GeminiPromptFeedback 安全拦截等提示
type GeminiPromptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

// ==================== 流式状态机 ====================

// OpenAIStreamState Gemini→OpenAI 流式转换的跨 chunk 状态（每请求独立实例，单 goroutine 顺序消费）
type OpenAIStreamState struct {
	ID             string
	Created        int64
	RoleSent       bool
	ToolCallIndex  int
	HasFunctionCall bool
	Usage          map[string]any // 缓存 usageMetadata，仅挂到最后一个 finish chunk
}

// ClaudeStreamState Gemini→Claude 流式转换的跨 chunk 状态
type ClaudeStreamState struct {
	MessageID           string
	ContentBlockIndex   int
	ThinkingBlockStarted bool
	ThinkingBlockStopped bool
	ThinkingBlockIndex  int
	TextBlockStarted    bool
	TextBlockStopped    bool
	TextBlockIndex      int
	HasToolUse          bool
	InputTokens         int64
	OutputTokens        int64
	MessageStartSent    bool
}

// ==================== 模型后缀解析结果 ====================

// SuffixInfo 模型名后缀剥离结果（-search/-code/-real/-fake/thinkingLevel）
type SuffixInfo struct {
	CleanModel       string
	ThinkingLevel    string // HIGH/LOW/MEDIUM/MINIMAL
	StreamingMode    string // "real"|"fake"|""
	ForceWebSearch   bool
	ForceCodeExecution bool
	HasStreamingSuffix bool
	HasThinkingSuffix  bool
}

// ConvertOpts 转换器与线路级开关解耦的选项
type ConvertOpts struct {
	ForceThinking      bool
	ForceWebSearch     bool
	ForceCodeExecution bool
	ForceUrlContext    bool
	SafetyThreshold    string // 默认 "OFF"
	HTTPImageClient    *http.Client // 图片 URL 下载用；nil 则跳过远程下载只留占位文本（Phase 2 接通）
	MaxOutputTokensFallback int    // Claude/Responses 缺 max_tokens 时兜底
}

// GeminiError 解析后的 Gemini 错误
type GeminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
	Details []any  `json:"details,omitempty"`
}