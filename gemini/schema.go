package gemini

import (
	"fmt"
	"strconv"
	"strings"
)

// ConvertSchemaToGemini 将 OpenAI/Claude 的 JSON Schema 转换为 Gemini 参数/响应 Schema 格式。
// 移植自 AIStudioToAPI FormatConverter._convertSchemaToGemini：
//   - 黑名单字段过滤（$schema/additionalProperties/$ref/const 等；isResponseSchema 时额外过滤
//     default/examples/$defs/id）
//   - anyOf 折叠：含 null → nullable；单个非空变体直接折叠；多个变体保留 anyOf
//   - type 数组处理：["string","null"]→nullable + STRING；多类型（响应）→ anyOf
//   - enum：响应 Schema 强制字符串化并补 type=STRING；工具保留原值
//
// isProperties=true 表示当前对象是"属性名→Schema"的映射，key 是属性名而非 Schema 关键字，不做过滤/特判。
func ConvertSchemaToGemini(obj any, isResponseSchema, isProperties bool) any {
	switch o := obj.(type) {
	case map[string]any:
		result := make(map[string]any, len(o))

		for key, val := range o {
			// 1. 黑名单过滤（仅当 key 是 Schema 关键字时）
			if !isProperties && isUnsupportedSchemaKey(key, isResponseSchema) {
				continue
			}

			// 2. anyOf 特判（仅 Schema 关键字层级）
			if key == "anyOf" && !isProperties {
				arr, ok := val.([]any)
				if !ok {
					continue
				}
				hasNull, nonNull := splitNullVariants(arr)
				if hasNull {
					result["nullable"] = true
				}
				switch len(nonNull) {
				case 0:
					continue // 仅 null 类型，不强制
				case 1:
					converted := ConvertSchemaToGemini(nonNull[0], isResponseSchema, false)
					if m, ok := converted.(map[string]any); ok {
						for k, v := range m {
							result[k] = v
						}
					} else if converted != nil {
						result["anyOf"] = converted
					}
					if hasNull {
						result["nullable"] = true
					}
					continue
				default:
					result["anyOf"] = convertVariantList(nonNull, isResponseSchema)
					continue
				}
			}

			// 3. type 特判（数组/nullable/大写化）
			if key == "type" && !isProperties {
				if arr, ok := val.([]any); ok {
					hasNull, nonNull := splitNullTypes(arr)
					if hasNull {
						result["nullable"] = true
					}
					switch len(nonNull) {
					case 1:
						result["type"] = strings.ToUpper(nonNull[0])
					case 0:
						result["type"] = "STRING"
					default:
						if isResponseSchema {
							variants := make([]any, 0, len(nonNull))
							for _, t := range nonNull {
								variants = append(variants, map[string]any{"type": strings.ToUpper(t)})
							}
							result["anyOf"] = variants
						} else {
							types := make([]any, 0, len(nonNull))
							for _, t := range nonNull {
								types = append(types, strings.ToUpper(t))
							}
							result["type"] = types
						}
					}
					continue
				}
				if s, ok := val.(string); ok {
					result["type"] = strings.ToUpper(s)
					continue
				}
				if m, ok := val.(map[string]any); ok {
					result["type"] = ConvertSchemaToGemini(m, isResponseSchema, false)
					continue
				}
				result["type"] = val
				continue
			}

			// 4. enum 特判
			if key == "enum" && !isProperties {
				if isResponseSchema {
					switch e := val.(type) {
					case []any:
						ss := make([]any, 0, len(e))
						for _, v := range e {
							ss = append(ss, toString(v))
						}
						result["enum"] = ss
					default:
						if val != nil {
							result["enum"] = []any{toString(val)}
						}
					}
					result["type"] = "STRING"
				} else {
					result["enum"] = val
				}
				continue
			}

			// 5. 递归
			if m, ok := val.(map[string]any); ok {
				nextIsProperties := key == "properties"
				recursionFlag := false
				if isProperties {
					recursionFlag = false
				} else {
					recursionFlag = nextIsProperties
				}
				result[key] = ConvertSchemaToGemini(m, isResponseSchema, recursionFlag)
			} else {
				result[key] = val
			}
		}

		// 6. enumDescriptions → description 文本
		if !isProperties {
			if ed, ok := o["enumDescriptions"].([]any); ok && len(ed) > 0 {
				var enumValues []any
				if ev, ok := result["enum"].([]any); ok {
					enumValues = ev
				}
				lines := make([]string, 0, len(ed))
				for idx, desc := range ed {
					d, ok := desc.(string)
					if !ok || d == "" {
						continue
					}
					label := "value " + itoa(idx+1)
					if idx < len(enumValues) {
						label = toString(enumValues[idx])
					}
					lines = append(lines, "- "+label+": "+d)
				}
				if len(lines) > 0 {
					text := "Enum descriptions:\n" + strings.Join(lines, "\n")
					if existing, ok := result["description"].(string); ok && existing != "" {
						result["description"] = existing + "\n\n" + text
					} else {
						result["description"] = text
					}
				}
			}
		}

		return result

	case []any:
		out := make([]any, 0, len(o))
		for _, v := range o {
			out = append(out, ConvertSchemaToGemini(v, isResponseSchema, false))
		}
		return out

	default:
		return obj
	}
}

// isUnsupportedSchemaKey 判断是否为需过滤的 Schema 关键字
func isUnsupportedSchemaKey(key string, isResponseSchema bool) bool {
	switch key {
	case "$schema", "additionalProperties", "ref", "$ref", "propertyNames",
		"patternProperties", "unevaluatedProperties", "exclusiveMinimum",
		"exclusiveMaximum", "const", "$comment", "enumDescriptions":
		return true
	}
	if isResponseSchema {
		switch key {
		case "default", "examples", "$defs", "id":
			return true
		}
	}
	return false
}

// splitNullVariants 拆分 anyOf 变体中的 null 变体
func splitNullVariants(arr []any) (hasNull bool, nonNull []any) {
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			if t, ok := m["type"].(string); ok && t == "null" {
				hasNull = true
				continue
			}
		}
		nonNull = append(nonNull, v)
	}
	return
}

// splitNullTypes 拆分 type 数组中的 "null"
func splitNullTypes(arr []any) (hasNull bool, nonNull []string) {
	for _, v := range arr {
		if s, ok := v.(string); ok {
			if s == "null" {
				hasNull = true
				continue
			}
			nonNull = append(nonNull, s)
		}
	}
	return
}

// convertVariantList 递归转换 anyOf 变体列表
func convertVariantList(variants []any, isResponseSchema bool) []any {
	out := make([]any, 0, len(variants))
	for _, v := range variants {
		out = append(out, ConvertSchemaToGemini(v, isResponseSchema, false))
	}
	return out
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
