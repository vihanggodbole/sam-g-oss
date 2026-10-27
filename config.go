package sam

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// Built-in defaults applied when a session setting is zero.
const (
	DefaultToolConcurrency = 4
	DefaultToolOutputLimit = 256 << 10
	DefaultTriggerPercent  = 90
	DefaultRecentPercent   = 20
	// DefaultMaxProviderEventBytes bounds one assembled provider SSE event.
	DefaultMaxProviderEventBytes = 32 << 20
)

// Config is the runtime configuration supplied to Open or Reload. Both snapshot its
// maps, slices, and tool schemas, so changing the caller's value afterwards
// has no effect. HTTPClient, Tool.Run and hook functions are shared, not copied: the host
// keeps owning those implementations. Provider credentials named by
// APIKeyEnv are still read from the environment at request time.
type Config struct {
	// SkillDirectories adds server-side skill roots after global defaults, before workspace skills.
	// Relative paths resolve against each workspace; ~/ resolves against the server user home.
	SkillDirectories []string `json:"skill_directories,omitempty"`
	// DatabasePath is the local SQLite file owned by this runtime.
	DatabasePath string `json:"database_path"`
	// Providers and Models are keyed by the aliases used in session settings.
	Providers map[string]Provider `json:"providers"`
	Models    map[string]Model    `json:"models"`
	// Defaults supplies inherited settings for existing and new sessions.
	Defaults SessionConfig `json:"defaults"`
	// MaxProviderEventBytes bounds one assembled provider SSE event (all
	// of its data lines). 0 means DefaultMaxProviderEventBytes (32 MiB). A
	// larger event fails the model request without retrying.
	MaxProviderEventBytes int `json:"max_provider_event_bytes,omitempty"`

	// HTTPClient is used for all model requests. Nil means a default client.
	// The host retains ownership of the client and its transport.
	HTTPClient *http.Client `json:"-"`
	// Tools adds host tools or replaces built-in tools with the same name.
	// Invalid registrations are skipped and reported by Runtime.Diagnostics.
	Tools []Tool `json:"-"`
	// Hooks supplies host lifecycle policies and persistent contributions.
	// Invalid registrations reject Open (before ownership) and Reload.
	Hooks Hooks `json:"-"`
	// Auth supplies subscription-account credentials for providers whose
	// Auth is set. The host opens and closes it; several runtimes in one
	// process may share it. Required only when such providers are configured.
	Auth *AuthManager `json:"-"`
	// DocumentationDir is an absolute path installed explicitly by the host
	// with InstallDocumentation. Open never writes documentation files.
	DocumentationDir string `json:"-"`
}

// Provider supplies a model API's endpoint and credentials. BaseURL is a
// versioned Responses base such as https://api.openai.com/v1, or a Copilot origin
// for the supported Messages profile. The client appends the API route. Credentials
// are supplied directly by APIKey or read from APIKeyEnv when a request starts,
// or, when Auth is set, come from the session's selected login account.
type Provider struct {
	// BaseURL is the API base; the model selects /responses or /v1/messages. For
	// AuthOpenAIChatGPT an empty value means https://api.openai.com/v1.
	// For AuthGitHubCopilot an empty value means the account's Copilot
	// endpoint reported at login (validated to be under githubcopilot.com).
	BaseURL string `json:"base_url,omitempty"`
	// Auth selects subscription login instead of an API key:
	// AuthOpenAIChatGPT or AuthGitHubCopilot. Sessions using such a provider
	// must select an account of that provider. Empty means API key.
	Auth string `json:"auth,omitempty"`
	// APIKeyEnv names the environment variable read for each model request.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// APIKey lets Go hosts pass a credential directly. It is never persisted.
	APIKey string `json:"-"`
}

// ModelAPI selects one of the two native wire formats. Empty means Responses.
type ModelAPI string

const (
	APIResponses         ModelAPI = "responses"
	APIAnthropicMessages ModelAPI = "anthropic_messages"
)

func (a ModelAPI) effective() ModelAPI {
	if a == "" {
		return APIResponses
	}
	return a
}

// Model is a configured model entry. Capabilities and the local context budget
// are declared by the host; discovery does not mutate this configuration.
type Model struct {
	// API is responses (also the legacy empty value) or anthropic_messages.
	API ModelAPI `json:"api,omitempty"`
	// MaxOutputTokens is a verified provider output bound, required for Messages.
	// Zero means unknown, not unlimited. It does not alter Responses requests.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// Provider names an entry in Config.Providers.
	Provider string `json:"provider"`
	// ModelID is the provider's API model identifier.
	ModelID string `json:"model_id"`
	// ContextTokens is the host-configured context budget used for estimates.
	ContextTokens int `json:"context_tokens"`
	// ReasoningEfforts and ReasoningSummaries declare supported options.
	ReasoningEfforts   []string `json:"reasoning_efforts,omitempty"`
	ReasoningSummaries bool     `json:"reasoning_summaries,omitempty"`
	// CatalogAccount marks an account-scoped managed entry. Nil means manual;
	// a pointer to empty string identifies an API-key catalog.
	CatalogAccount *string `json:"catalog_account,omitempty"`
	// NoImages records that the provider reports this model cannot read
	// images; prompts with images are then refused instead of failing later.
	NoImages bool `json:"no_images,omitempty"`
	// Unavailable preserves a discovered entry that cannot execute here.
	Unavailable string `json:"unavailable,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// SessionConfig is a concrete effective configuration and the shared defaults
// type. Zero numeric fields use built-in defaults; negative values are invalid.
// Relative prompt paths resolve against the workspace. Per-session choices use
// SessionOverrides, so explicit zero differs from inheritance.
type SessionConfig struct {
	// Model is a configured model alias. The first accepted prompt or steering
	// message binds the session to that model identity; SwitchModel explicitly
	// changes the binding for future work without rewriting history.
	Model string `json:"model"`
	// Reasoning options must be declared by the selected model.
	ReasoningEffort  string `json:"reasoning_effort"`
	ReasoningSummary bool   `json:"reasoning_summary"`
	// Instructions are appended after the built-in prompt. File contents are
	// prepared at setup or explicit unified reload, never reread by workers.
	AppendInstructions     string `json:"append_instructions"`
	AppendInstructionsFile string `json:"append_instructions_file"`
	// DisabledTools names registered tools not offered to the model.
	DisabledTools []string `json:"disabled_tools"`
	// ToolConcurrency limits parallel calls in one response batch across tools.
	ToolConcurrency int `json:"tool_concurrency"`
	// ToolOutputLimit is the maximum captured plus returned bytes per tool call.
	ToolOutputLimit int `json:"tool_output_limit"`
	// Account is the login account used when the model's provider has Auth
	// set; it must be empty for API-key providers. Used chats change model and
	// account only through SwitchModel. Each turn pins the account's current
	// login generation; reauthorization under that label is a separate auth action.
	Account string `json:"account"`
	// Compaction controls context summarization.
	Compaction CompactionConfig `json:"compaction"`
}

// CompactionConfig selects the summarization model, prompt, and context budgets.
// An empty Model uses the session's main model. Prompt and PromptFile are
// mutually exclusive. Prompt-file contents are prepared at setup or unified
// reload; unreadable files reject preparation without changing installed state.
type CompactionConfig struct {
	// Model is a configured summarization alias; empty means the main model.
	Model string `json:"model"`
	// Account is the login account for a compaction model whose provider has
	// Auth set. Empty means the session's Account when both models use the
	// same login provider.
	Account string `json:"account"`
	// Prompt and PromptFile are alternative extra summarization instructions.
	Prompt     string `json:"prompt"`
	PromptFile string `json:"prompt_file"`
	// DisableAuto disables automatic compaction at model-step boundaries.
	DisableAuto bool `json:"disable_auto"`
	// OutputReserve is withheld from the input budget; zero uses the built-in
	// context-based default. Messages also requests it as a hard output cap
	// (including thinking), validated against the model's MaxOutputTokens.
	OutputReserve int `json:"output_reserve"`
	// TriggerPercent starts automatic compaction at this share of input budget.
	TriggerPercent int `json:"trigger_percent"`
	// RecentPercent is the minimum input-budget share retained in whole recent
	// items. Tool/reasoning groups and a protected preceding user can add more.
	RecentPercent int `json:"recent_percent"`
}

// ModelBinding pins the current main-model identity. Only explicit SwitchModel
// can change a used session's binding; configuration reload cannot redirect it.
type ModelBinding struct {
	Model    string   `json:"model"`
	Provider string   `json:"provider"`
	BaseURL  string   `json:"base_url"`
	ModelID  string   `json:"model_id"`
	API      ModelAPI `json:"api,omitempty"`
}

// LoadConfig reads a JSON runtime configuration file. Unknown fields are
// ignored so hosts can keep their own settings in the same file. Credential
// values should not be stored in JSON; use Provider.APIKeyEnv where possible.
func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, errorf(CodeInvalidArgument, "read config: %v", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, errorf(CodeInvalidArgument, "parse config %s: %v", path, err)
	}
	return c, nil
}

// clone returns a copy that shares no mutable maps, slices, or schema bytes
// with c. The HTTP client and tool/hook functions are shared by design.
func (c Config) clone() Config {
	c.SkillDirectories = slices.Clone(c.SkillDirectories)
	c.Hooks = c.Hooks.clone()
	c.Providers = maps.Clone(c.Providers)
	models := make(map[string]Model, len(c.Models))
	for name, m := range c.Models {
		m.ReasoningEfforts = slices.Clone(m.ReasoningEfforts)
		if m.CatalogAccount != nil {
			m.CatalogAccount = new(*m.CatalogAccount)
		}
		models[name] = m
	}
	c.Models = models
	c.Defaults.DisabledTools = slices.Clone(c.Defaults.DisabledTools)
	c.Tools = slices.Clone(c.Tools)
	for i := range c.Tools {
		c.Tools[i].Parameters = bytes.Clone(c.Tools[i].Parameters)
	}
	return c
}

func (c *Config) validate() error {
	for _, path := range c.SkillDirectories {
		if strings.TrimSpace(path) == "" {
			return errorf(CodeInvalidArgument, "skill_directories entries must not be empty")
		}
	}
	if err := c.Hooks.validate(); err != nil {
		return err
	}
	if c.DatabasePath == "" {
		return errorf(CodeInvalidArgument, "database_path is required")
	}
	if c.DocumentationDir != "" && !filepath.IsAbs(c.DocumentationDir) {
		return errorf(CodeInvalidArgument, "documentation directory must be absolute")
	}
	if c.MaxProviderEventBytes < 0 {
		return errorf(CodeInvalidArgument, "max_provider_event_bytes must not be negative (0 means the 32 MiB default)")
	}
	for name, p := range c.Providers {
		if err := c.validateProvider(name, &p); err != nil {
			return err
		}
		c.Providers[name] = p
	}
	return c.validateModels()
}

func (c Config) validateModels() error {
	for name, m := range c.Models {
		if _, ok := c.Providers[m.Provider]; !ok {
			return errorf(CodeInvalidArgument, "model %q references unknown provider %q", name, m.Provider)
		}
		if err := validateModelAPI(m, c.Providers[m.Provider]); err != nil {
			return errorf(ErrorCodeOf(err), "model %q: %v", name, err)
		}
		if m.ModelID == "" || m.ContextTokens <= 0 {
			return errorf(CodeInvalidArgument, "model %q needs model_id and positive context_tokens", name)
		}
	}
	return nil
}

// validateProvider uses the same target rules as standalone discovery.
func (c *Config) validateProvider(name string, p *Provider) error {
	normalized, err := normalizeProvider(*p)
	if err != nil {
		return err
	}
	if p.Auth != "" && (c.Auth == nil || !c.Auth.flowConfigured(p.Auth)) {
		return errorf(CodeInvalidArgument, "provider %q uses subscription login but its AuthManager flow is not configured", name)
	}
	*p = normalized
	return nil
}

// resolvedModel is a model entry joined with its provider. account is the
// login account of one operation; it is not part of the model identity.
type resolvedModel struct {
	Alias    string
	Provider string
	BaseURL  string
	Model
	apiKeyEnv string
	apiKey    string
	auth      string
	account   accountRef
}

func (m resolvedModel) binding() ModelBinding {
	return ModelBinding{Model: m.Alias, Provider: m.Provider, BaseURL: m.BaseURL, ModelID: m.ModelID, API: m.API.effective()}
}

func (m resolvedModel) credential() string {
	if m.apiKey != "" {
		return m.apiKey
	}
	if m.apiKeyEnv != "" {
		return os.Getenv(m.apiKeyEnv)
	}
	return ""
}

func (m resolvedModel) supportsReasoning() bool {
	return m.API.effective() == APIAnthropicMessages || len(m.ReasoningEfforts) > 0 || m.ReasoningSummaries
}

// outputReserve is the configured reserve or min(8192, context/4).
func (m resolvedModel) outputReserve(configured int) int {
	if configured > 0 {
		return configured
	}
	return min(8192, m.ContextTokens/4)
}

func (r *preparedConfig) resolve(alias string) (resolvedModel, error) {
	m, ok := r.cfg.Models[alias]
	if !ok {
		return resolvedModel{}, errorf(CodeInvalidArgument, "model %q is not configured", alias)
	}
	if m.Unavailable != "" {
		return resolvedModel{}, errorf(CodeUnsupported, "model %q is unavailable: %s", alias, m.Unavailable)
	}
	p := r.cfg.Providers[m.Provider]
	return resolvedModel{
		Alias: alias, Provider: m.Provider, BaseURL: strings.TrimRight(p.BaseURL, "/"), Model: m,
		apiKeyEnv: p.APIKeyEnv, apiKey: p.APIKey, auth: p.Auth,
	}, nil
}

// withAccount pins the current login of account name for one operation of
// m. A saved account is never ignored if the host changes authentication mode.
func (r *preparedConfig) withAccount(m resolvedModel, name string) (resolvedModel, error) {
	m.account = accountRef{name: name}
	if err := r.checkAccount(m, name, "account"); err != nil {
		return m, errorf(CodeAuthRequired, "%s. No request was sent. Restore the OAuth provider configuration or use idle SwitchModel to explicitly select a valid model/account", err.Error())
	}
	if m.auth == "" {
		return m, nil
	}
	ref, err := r.cfg.Auth.ref(m.auth, name)
	if err == nil {
		m.account = ref
	}
	return m, err
}

// accountName preserves the selected account even in a failed operation whose
// provider configuration no longer supports it. Normal API-key models have none.
func (m resolvedModel) accountName() string { return m.account.name }

// provenance names the model and, for login providers, the account.
func (m resolvedModel) provenance() string {
	if m.auth == "" {
		return m.Alias + "/" + m.ModelID
	}
	return m.Alias + "/" + m.ModelID + "@" + m.account.name
}

// compactionAccount is the account of compaction model comp: its own
// setting, or the session's account when both models use the same login.
func (c SessionConfig) compactionAccount(main, comp resolvedModel) string {
	if c.Compaction.Account != "" || comp.auth != main.auth {
		return c.Compaction.Account
	}
	return c.Account
}

// checkAccount validates an account selection for model m. A name that is
// not logged in yet is accepted (it fails when an operation runs); a logged
// in account of another provider is rejected now.
func (r *preparedConfig) checkAccount(m resolvedModel, name, field string) error {
	if m.CatalogAccount != nil && name != *m.CatalogAccount {
		return errorf(CodeInvalidArgument, "model %q was installed for account %q; select that account", m.Alias, *m.CatalogAccount)
	}
	switch {
	case m.auth == "" && name != "":
		return errorf(CodeInvalidArgument, "%s %q: model %q uses an API-key provider, which has no login accounts", field, name, m.Alias)
	case m.auth == "":
		return nil
	case name == "":
		return errorf(CodeInvalidArgument, "model %q uses %s login; set %s to a logged-in %s account", m.Alias, m.auth, field, m.auth)
	case !validAccountName(name):
		return errorf(CodeInvalidArgument, "%s %q is not a valid account name", field, name)
	}
	if kind, ok := r.cfg.Auth.kindOf(name); ok && kind != m.auth {
		return errorf(CodeInvalidArgument, "%s %q is a %s account, but model %q needs a %s account", field, name, kind, m.Alias, m.auth)
	}
	return nil
}

// resolveBound resolves the session's main model and verifies it still
// matches the frozen binding.
func (r *preparedConfig) resolveBound(s *sessionRow) (resolvedModel, error) {
	if r.modelUnavailable(s.cfg.Model) {
		return resolvedModel{}, errorf(CodeUnsupported, "model %q is no longer available; choose a replacement model for this chat", s.cfg.Model)
	}
	m, err := r.resolve(s.cfg.Model)
	if s.binding == nil {
		return m, err
	}
	bound := *s.binding
	bound.API = bound.API.effective()
	if err != nil || m.binding() != bound {
		return m, errorf(CodeConflict,
			"session is bound to model %q (%s at %s); that alias is missing or now resolves differently. Restore the original configuration or explicitly switch this session's model",
			s.binding.Model, s.binding.ModelID, s.binding.BaseURL)
	}
	return m, nil
}

// Stored selections can outlive their catalog. They remain readable, but new
// sessions and work still use strict validateSessionConfig before admission.
func (r *preparedConfig) modelUnavailable(alias string) bool {
	m, ok := r.cfg.Models[alias]
	return !ok || m.Unavailable != ""
}

func (r *preparedConfig) validateStoredConfig(c SessionConfig) error {
	if r.modelUnavailable(c.Model) || c.Compaction.Model != "" && r.modelUnavailable(c.Compaction.Model) {
		return r.validateSessionValues(c)
	}
	return r.validateSessionConfig(c)
}

// An already mismatched binding after reopen is read-only for work, not for
// unrelated settings repairs. Keep rejecting any new redirection introduced by
// this candidate. SwitchModel remains the only operation that changes binding.
func (r *preparedConfig) validateExistingSession(s *sessionRow, current *preparedConfig) error {
	if !r.modelUnavailable(s.cfg.Model) {
		if _, err := r.resolveBound(s); err != nil {
			before, oldErr := current.resolve(s.cfg.Model)
			after, newErr := r.resolve(s.cfg.Model)
			if oldErr != nil || newErr != nil || before.binding() != after.binding() {
				return err
			}
			return r.validateSessionValues(s.cfg)
		}
	}
	return r.validateStoredConfig(s.cfg)
}

// Editing a retired chat may repair its other settings without first resolving
// its missing model. Newly selected references must still be usable.
func (r *preparedConfig) validateModelEdits(previous, next SessionConfig) error {
	if previous.Model != next.Model {
		if _, err := r.resolve(next.Model); err != nil {
			return err
		}
	}
	if previous.Compaction.Model != next.Compaction.Model && next.Compaction.Model != "" {
		if _, err := r.resolve(next.Compaction.Model); err != nil {
			return err
		}
	}
	return nil
}

// withDefaults fills zero values with built-in defaults.
func (c SessionConfig) withDefaults() SessionConfig {
	if c.ToolConcurrency <= 0 {
		c.ToolConcurrency = DefaultToolConcurrency
	}
	if c.ToolOutputLimit <= 0 {
		c.ToolOutputLimit = DefaultToolOutputLimit
	}
	if c.Compaction.TriggerPercent <= 0 {
		c.Compaction.TriggerPercent = DefaultTriggerPercent
	}
	if c.Compaction.RecentPercent <= 0 {
		c.Compaction.RecentPercent = DefaultRecentPercent
	}
	return c
}

func (r *preparedConfig) validateSessionConfig(c SessionConfig) error {
	if err := r.validateSessionValues(c); err != nil {
		return err
	}
	m, err := r.resolve(c.Model)
	if err != nil {
		return err
	}
	cm := m
	if c.Compaction.Model != "" {
		if cm, err = r.resolve(c.Compaction.Model); err != nil {
			return err
		}
	}
	if err := r.checkAccount(m, c.Account, "account"); err != nil {
		return err
	}
	if err := r.checkAccount(cm, c.compactionAccount(m, cm), "compaction.account"); err != nil {
		return err
	}
	return validateModelOptions(c, m, cm)
}

func validateModelOptions(c SessionConfig, m, cm resolvedModel) error {
	if c.ReasoningEffort != "" && !slices.Contains(m.ReasoningEfforts, c.ReasoningEffort) {
		return errorf(CodeUnsupported, "model %q does not support reasoning effort %q", c.Model, c.ReasoningEffort)
	}
	if c.ReasoningSummary && !m.ReasoningSummaries {
		return errorf(CodeUnsupported, "model %q does not expose reasoning summaries", c.Model)
	}
	cc := c.Compaction
	for _, rm := range []resolvedModel{m, cm} {
		if err := validateMessagesCap(rm, rm.outputReserve(cc.OutputReserve)); err != nil {
			return err
		}
		if cc.OutputReserve >= rm.ContextTokens {
			return errorf(CodeInvalidArgument, "compaction.output_reserve %d leaves no input context for model %q (context_tokens %d); "+
				"use a smaller reserve, or 0 for min(8192, context/4)", cc.OutputReserve, rm.Alias, rm.ContextTokens)
		}
	}
	return nil
}

func (r *preparedConfig) validateSessionValues(c SessionConfig) error {
	if !utf8.ValidString(c.AppendInstructions) || !utf8.ValidString(c.Compaction.Prompt) {
		return errorf(CodeInvalidArgument, "configured prompt text must be valid UTF-8")
	}
	if c.Compaction.Prompt != "" && c.Compaction.PromptFile != "" {
		return errorf(CodeInvalidArgument, "set compaction prompt or prompt_file, not both")
	}
	for _, name := range c.DisabledTools {
		if _, ok := r.tools[name]; !ok && !r.skipped[name] {
			return errorf(CodeInvalidArgument, "unknown tool %q", name)
		}
	}
	cc := c.Compaction
	if c.ToolConcurrency < 0 || c.ToolOutputLimit < 0 || cc.OutputReserve < 0 ||
		cc.TriggerPercent < 0 || cc.TriggerPercent > 100 || cc.RecentPercent < 0 || cc.RecentPercent > 100 {
		return errorf(CodeInvalidArgument, "numeric settings must be non-negative and percentages at most 100 (0 means the default)")
	}
	return nil
}

// readTextFile reads an explicitly configured UTF-8 prompt file.
func readTextFile(workspace, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", errorf(CodeInvalidArgument, "read configured file: %v", err)
	}
	if !utf8.Valid(b) {
		return "", errorf(CodeInvalidArgument, "configured file %s is not valid UTF-8", path)
	}
	return string(b), nil
}
