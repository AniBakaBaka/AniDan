// SPDX-License-Identifier: AGPL-3.0-only
package server

import "strings"

const responseLoggingDescription = "AniDan: opt-in bounded sanitized response diagnostics in app.log. JSON strings and unknown fields are omitted; non-JSON, incomplete, oversized or over-complex bodies are omitted whole. Format: sanitized-json-structure or omitted. This is not literal raw-byte capture. Limits: 64 KiB input, 8 KiB representation, depth 8, 256 nodes, 64 entries per collection."

// Serving-time overlay only: the pinned upstream contract stays unchanged.
func nativeResponseLoggingDescription(path, method, description string) string {
	switch {
	case path == "/api/control/scrapers/{provider}" && strings.EqualFold(method, "put"):
		return strings.ReplaceAll(description, "是否记录原始响应到日志文件", "是否启用有界脱敏响应诊断") + "\n\n" + responseLoggingDescription
	case path == "/api/control/logs/files" && strings.EqualFold(method, "get"):
		return "列出实际存在的历史日志文件。AniDan 源站/元数据响应诊断写入 app.log，使用应用文件轮转；不会单独创建 scraper_responses.log 或 metadata_responses.log。迁移/外部工具保留的其他文件可能仍在列表中。\n\n" + responseLoggingDescription
	}
	return description
}

// Call only on a cloned/resolved schema, never the cached upstream reference.
func annotateResponseLoggingSchema(value any) {
	switch v := value.(type) {
	case map[string]any:
		if properties, ok := v["properties"].(map[string]any); ok {
			if field, ok := properties["logRawResponses"].(map[string]any); ok {
				field["description"] = responseLoggingDescription
			}
		}
		for _, child := range v {
			annotateResponseLoggingSchema(child)
		}
	case []any:
		for _, child := range v {
			annotateResponseLoggingSchema(child)
		}
	}
}
