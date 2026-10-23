package sam

// ModelReference is versioned, advisory model metadata. It is NOT a capability
// or limit reported by the selected account/route. In particular, Pi's Codex
// route differs from ChatGPT token-sharing's public Responses route. Hosts
// must not silently install these values as execution configuration.
type ModelReference struct {
	Source            string `json:"source"`
	ContextTokens     int    `json:"context_tokens"`
	SupportsReasoning bool   `json:"supports_reasoning"`
}

type referenceMetadata struct {
	context   int
	reasoning bool
}

// Exact-ID snapshot from pi-ai 0.87.1. Source hashes and update instructions
// live in docs/MODEL-METADATA.md. This is not an availability catalog: entries
// never add models to discovery or change account policy, transport or budgets.
var chatGPTReference = map[string]referenceMetadata{
	"gpt-5.3-codex-spark": {128000, true},
	"gpt-5.5":             {272000, true},
	"gpt-5.6-luna":        {272000, true},
	"gpt-5.6-sol":         {272000, true},
	"gpt-5.6-terra":       {272000, true},
	"gpt-6-astra":         {272000, true},
	"gpt-6-luna":          {272000, true},
	"gpt-6-sol":           {272000, true},
}

var copilotReference = map[string]referenceMetadata{
	"claude-fable-5":          {1000000, true},
	"claude-fable-5.1":        {1000000, true},
	"claude-haiku-4.5":        {200000, true},
	"claude-opus-4.7":         {1000000, true},
	"claude-opus-4.8":         {1000000, true},
	"claude-opus-5":           {1000000, true},
	"claude-opus-5.5":         {1000000, true},
	"claude-sonnet-4.6":       {1000000, true},
	"claude-sonnet-5":         {1000000, true},
	"gemini-3.5-flash":        {200000, true},
	"gemini-3.6-flash":        {1000000, true},
	"gemini-3.7-flash":        {1000000, true},
	"gemini-3.8-flash":        {1000000, true},
	"kimi-k2.7-code":          {256000, true},
	"kimi-k3":                 {1048576, true},
	"gpt-5-mini":              {264000, true},
	"gpt-5.3-codex":           {1000000, true},
	"gpt-5.4":                 {1000000, true},
	"gpt-5.4-mini":            {400000, true},
	"gpt-5.4-nano":            {400000, true},
	"gpt-5.5":                 {1000000, true},
	"gpt-5.6-luna":            {1050000, true},
	"gpt-5.6-sol":             {1050000, true},
	"gpt-5.6-terra":           {1050000, true},
	"gpt-6-astra":             {1000000, true},
	"gpt-6-luna":              {1000000, true},
	"gpt-6-sol":               {1000000, true},
	"grok-4.5":                {500000, true},
	"grok-4.6":                {500000, true},
	"grok-4.7":                {500000, true},
	"mai-code-1-flash-picker": {256000, true},
	"mai-code-1.1-flash":      {256000, true},
}

func modelReference(auth, id string) *ModelReference {
	var entry referenceMetadata
	var source string
	switch auth {
	case AuthOpenAIChatGPT:
		entry, source = chatGPTReference[id], "pi-ai 0.87.1 / openai-codex"
	case AuthGitHubCopilot:
		entry, source = copilotReference[id], "pi-ai 0.87.1 / github-copilot"
	}
	if entry.context == 0 {
		return nil
	}
	return &ModelReference{Source: source, ContextTokens: entry.context, SupportsReasoning: entry.reasoning}
}
