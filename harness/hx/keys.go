package hx

// Service keys — the stable positions each capability occupies on runtime.Context.
// A consumer reads ctx.Get(KeyTools) and casts to ToolRuntime; a provider
// registers under the same key. Keys are the ONLY coupling between spine packages.
const (
	KeyLLM          = "llm"          // LlmRuntime
	KeySession      = "session"      // SessionStore
	KeyPrompt       = "systemPrompt" // SystemPrompt
	KeyTools        = "tools"        // ToolRuntime
	KeyAgents       = "agents"       // AgentRegistry
	KeyAgentLoop    = "agentLoop"    // AgentFactory
	KeyCompaction   = "compaction"   // CompactionEngine (deferred)
	KeySandbox      = "sandbox"      // sandbox provider (H2)
	KeySubprocess   = "subprocess"   // subprocess runtime (H2)
	KeyFS           = "fs"           // filesystem (H2)
	KeyShell        = "shell"        // shell executor (H2)
	KeyEvidenceSink = "evidenceSink" // bridge: tool/result facts → collectors (H3)
)
