// Package proto freezes the agent-facing contracts for P0: the MCP tool
// surface WittyChat exposes to assistants, and the ACP client-side types
// the runtime bridge will speak. Runtime wiring lands post-P0.
//
// 冻结 Agent 契约：WittyChat 对助理暴露的 MCP 工具面 + ACP 客户端类型。
package proto

// ---------- MCP tool surface / MCP 工具面 ----------
// WittyChat acts as an MCP *server*; assistants call these tools.

// Tool describes one MCP-callable capability.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Scope       string         `json:"scope"`       // required assistant scope
	InputSchema map[string]any `json:"inputSchema"` // JSON Schema object
}

// Tools is the frozen P0 tool list.
var Tools = []Tool{
	{
		Name: "im.send_message", Scope: "im.send",
		Description: "Send a markdown message to a conversation the assistant's owner is in.",
		InputSchema: obj(map[string]any{
			"conv_id":  str(),
			"markdown": str(),
		}, "conv_id", "markdown"),
	},
	{
		Name: "im.read_messages", Scope: "im.read",
		Description: "Read ops from a conversation; delta by op_seq watermark.",
		InputSchema: obj(map[string]any{
			"conv_id":   str(),
			"after_seq": num(),
			"limit":     num(),
		}, "conv_id"),
	},
	{
		Name: "im.search", Scope: "im.read",
		Description: "Full-text search messages in conversations the owner can see.",
		InputSchema: obj(map[string]any{"q": str(), "limit": num()}, "q"),
	},
	{
		Name: "objects.upload", Scope: "objects.write",
		Description: "Upload a file; returns object_id attachable to messages/mail.",
		InputSchema: obj(map[string]any{
			"name": str(), "mime": str(), "content_b64": str(),
		}, "name", "content_b64"),
	},
	{
		Name: "objects.fetch", Scope: "objects.read",
		Description: "Download object bytes (base64) the owner can access.",
		InputSchema: obj(map[string]any{"object_id": str()}, "object_id"),
	},
	{
		Name: "mail.send", Scope: "mail.send",
		Description: "Send email as the owner's primary address (approval flow unless auto_send).",
		InputSchema: obj(map[string]any{
			"to": arr(str()), "cc": arr(str()),
			"subject": str(), "body": str(),
		}, "to", "subject", "body"),
	},
	{
		Name: "mail.search", Scope: "mail.read",
		Description: "Search the owner's mailbox.",
		InputSchema: obj(map[string]any{"q": str(), "limit": num()}, "q"),
	},
	{
		Name: "approval.request", Scope: "",
		Description: "Ask the owner to approve an action; blocks until decided.",
		InputSchema: obj(map[string]any{
			"action": str(), "detail": obj(map[string]any{}),
		}, "action"),
	},
}

// ---------- ACP client types / ACP 客户端类型 ----------
// Subset of agent-client-protocol schema needed for the future runtime bridge.

type SessionConfig struct {
	WorkDir      string            `json:"workdir,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	MCPServers   []MCPServerSpec   `json:"mcpServers,omitempty"`
	SystemPrompt string            `json:"systemPrompt,omitempty"`
}

type MCPServerSpec struct {
	Name    string            `json:"name"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"` // http transport / HTTP 传输
}

type PromptContentBlock struct {
	Type string `json:"type"` // text | resource | image …
	Text string `json:"text,omitempty"`
}

type AgentEvent struct {
	SessionID string         `json:"sessionId"`
	Kind      string         `json:"kind"` // message_chunk|tool_call|plan|done|error
	Payload   map[string]any `json:"payload,omitempty"`
}

// ---------- approval flow / 审批流 ----------
// Assistants' send actions route through approval unless auto_send.

type ApprovalRequest struct {
	ID          string         `json:"id"`
	AssistantID string         `json:"assistant_id"`
	OwnerID     string         `json:"owner_id"`
	Action      string         `json:"action"` // im.send_message | mail.send | objects.upload
	Payload     map[string]any `json:"payload"`
	Status      string         `json:"status"` // pending|approved|rejected|expired
	CreatedAt   int64          `json:"created_at"`
	DecidedAt   int64          `json:"decided_at,omitempty"`
	DecidedBy   string         `json:"decided_by,omitempty"`
}

// schema helpers
func str() map[string]any { return map[string]any{"type": "string"} }
func num() map[string]any { return map[string]any{"type": "number"} }
func arr(items map[string]any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
