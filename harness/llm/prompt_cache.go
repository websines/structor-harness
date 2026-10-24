package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/websines/structor-harness/harness/hx"
)

type oaiCacheOptions struct {
	Mode string `json:"mode"`
	TTL  string `json:"ttl"`
}

type oaiTextPart struct {
	Type       string              `json:"type"`
	Text       string              `json:"text"`
	Breakpoint *oaiCacheBreakpoint `json:"prompt_cache_breakpoint,omitempty"`
}

type oaiCacheBreakpoint struct {
	Mode string `json:"mode"`
}

// CacheScopeKey separates cache accounting by an operator-owned namespace and
// verified principal, without exposing paths, user identities, or credentials.
// Do not use per-request or per-session randomness: related runs must share it.
func CacheScopeKey(namespace, principal string) string {
	digest := sha256.Sum256([]byte(namespace + "\x00" + principal))
	return "forge-native-v1:" + hex.EncodeToString(digest[:])
}

func explicitCacheModel(model string) bool {
	return model == "gpt-5.6" || strings.HasPrefix(model, "gpt-5.6-") ||
		strings.HasPrefix(model, "gpt-6-") || strings.HasPrefix(model, "gpt-6.1-")
}

func (a *OpenAIAdapter) applyPromptCache(req *oaiRequest, opts hx.GenerateOptions) {
	if !a.NativeCaching || strings.TrimRight(a.BaseURL, "/") != "https://api.openai.com/v1" {
		return
	}
	modern := explicitCacheModel(req.Model)
	// Unknown and compatible models receive no unsupported parameters. Earlier
	// OpenAI families retain automatic caching and their organization retention
	// default; a routing key is the only additional parameter for those families.
	if !modern && !strings.HasPrefix(req.Model, "gpt-5") &&
		!strings.HasPrefix(req.Model, "gpt-4.1") && !strings.HasPrefix(req.Model, "gpt-4o") {
		return
	}
	req.PromptCacheKey = a.CacheScope
	sort.Slice(req.Tools, func(i, j int) bool { return req.Tools[i].Function.Name < req.Tools[j].Function.Name })
	if !modern {
		return
	}
	mode := "explicit"
	if opts.PromptCache != nil && opts.PromptCache.ReuseHistory {
		mode = "implicit"
	}
	req.PromptCacheOptions = &oaiCacheOptions{Mode: mode, TTL: "30m"}
	if opts.PromptCache == nil || opts.PromptCache.SystemPrefix == "" ||
		!strings.HasPrefix(opts.System, opts.PromptCache.SystemPrefix) {
		return
	}
	prefix := opts.PromptCache.SystemPrefix
	leading := []oaiMessage{{
		Role: "developer",
		Content: []oaiTextPart{{
			Type: "text", Text: prefix, Breakpoint: &oaiCacheBreakpoint{Mode: "explicit"},
		}},
	}}
	fresh := opts.System[len(prefix):]
	history := req.Messages[1:]
	if task := opts.PromptCache.Task; task != "" {
		anchor := "\n\nCurrent authorized task (retain this intent across compaction):\n" + task
		if strings.HasSuffix(fresh, anchor) {
			fresh = strings.TrimSuffix(fresh, anchor)
			leading = append(leading, oaiMessage{
				Role: "developer",
				Content: []oaiTextPart{{
					Type: "text", Text: anchor, Breakpoint: &oaiCacheBreakpoint{Mode: "explicit"},
				}},
			})
			// The exact same authorized task was also the initial user message.
			// Keep it once at developer priority, including after compaction. Do
			// not remove followups, partial matches, or changed instructions.
			if len(history) > 0 && history[0].Role == "user" && history[0].Content == task {
				history = history[1:]
			}
		}
	}
	req.Messages = append(leading, history...)
	// Record/context/workspace updates stay at developer priority after history.
	// They cannot invalidate the cached task/history, and avoid a cache-write
	// premium. Immutable task packets above remain cacheable even when large.
	// The complete system text and split metadata are retained in the audit log.
	if fresh != "" {
		req.Messages = append(req.Messages, oaiMessage{Role: "developer", Content: fresh})
	}
}
