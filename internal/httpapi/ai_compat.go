package httpapi

// 这些接口在 V2 里由 LLM/Agent 与 MCP 工具链提供（OpenAI 兼容对话、Anthropic 消息、
// MCP 工具列表与调用）。本项目没有接入任何模型提供方，也没有 MCP 工具注册表，
// 因此统一明确拒绝，而不是返回伪造的补全结果。

const (
	messageNoLLM   = "未配置 AI 模型，无法提供该接口"
	messageNoMCP   = "未接入 MCP 工具链"
	messageNoAgent = "未启用 AI Agent，无法提供该接口"
)
