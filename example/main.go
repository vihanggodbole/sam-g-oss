// Command example serves a local-only multi-account coding/chat page.
// Run from the repository root: go run ./example
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	sam "github.com/vihanggodbole/sam-g-oss"
	"github.com/vihanggodbole/sam-g-oss/internal/hostpath"
)

const accountName = "personal"

// One local user can run several sessions. Selection belongs to each browser
// tab, not this host. modelMu owns model candidate/reload/save/publication.
type app struct {
	modelMu   sync.Mutex
	auth      *sam.AuthManager
	rt        *sam.Runtime
	cfg       sam.Config // immutable after Open except Models, guarded by modelMu
	origin    string
	csrf      string
	workspace string
}

func main() {
	port := flag.Int("port", 8765, "local HTTP port (0 chooses a free port)")
	data := flag.String("data", "~/.sam-g/example", "private example data directory; do not point at another sam-g store")
	clientID := os.Getenv("SAM_G_COPILOT_CLIENT_ID")
	copilot := flag.String("copilot-client-id", clientID, "optional override for the bundled Copilot Plugin client ID used by Pi")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *port, *data, *copilot); err != nil {
		fmt.Fprintln(os.Stderr, "Example:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, port int, data, copilotClientID string) error {
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer ln.Close()
	a, err := openApp(ctx, data, "http://"+ln.Addr().String(), copilotClientID, nil)
	if err != nil {
		return err
	}
	defer a.close()
	srv := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	fmt.Printf("Open %s in your browser.\nData: %s\nCtrl+C stops the example. Opening the page never starts model work.\n", a.origin, filepath.Dir(a.cfg.DatabasePath))
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpDone := make(chan error, 1)
	go func() { httpDone <- srv.Shutdown(shutdown) }()
	// Close also ends SSE subscriptions. Waiting for HTTP shutdown first
	// would spend the whole grace period waiting for the browser's stream.
	runtimeErr := a.rt.Close(shutdown)
	httpErr := <-httpDone
	if runtimeErr != nil || httpErr != nil {
		_ = srv.Close()
	}
	return errors.Join(runtimeErr, httpErr)
}

func openApp(ctx context.Context, data, origin, copilotClientID string, client *http.Client) (*app, error) {
	data, err := hostpath.Resolve(data)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(data)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("data directory must be owner-only: chmod 700 %q", dir)
	}
	authConfig := sam.AuthConfig{
		DatabasePath:  filepath.Join(dir, "auth", "auth.db"),
		OpenAI:        &sam.OpenAIChatGPTAuth{AgentName: "sam-g-example"},
		GitHubCopilot: &sam.GitHubCopilotAuth{ClientID: copilotClientID}, HTTPClient: client,
	}
	auth, err := sam.OpenAuth(authConfig)
	if err != nil {
		return nil, err
	}
	workspace, err := os.Getwd()
	if err != nil {
		_ = auth.Close()
		return nil, err
	}
	a := &app{auth: auth, origin: origin, csrf: rand.Text(), workspace: workspace}
	a.cfg = sam.Config{
		DatabasePath: filepath.Join(dir, "chats.db"), Auth: auth, HTTPClient: client,
		Providers: map[string]sam.Provider{"chatgpt": {Auth: sam.AuthOpenAIChatGPT}, "copilot": {Auth: sam.AuthGitHubCopilot}},
		Models:    map[string]sam.Model{},
		Defaults: sam.SessionConfig{
			Account:       accountName,
			DisabledTools: disabledTools(false),
			Compaction:    sam.CompactionConfig{DisableAuto: true},
		},
	}
	if err := a.loadModels(ctx); err != nil {
		_ = auth.Close()
		return nil, err
	}
	a.rt, err = sam.Open(ctx, a.cfg)
	if err != nil {
		_ = auth.Close()
		return nil, err
	}
	return a, nil
}

// Restore selected configurations from session bindings, never a catalog cache.
// New aliases include the provider so equal upstream IDs cannot share a binding.
// Used only to upgrade the earlier example when models.json is absent.
// Open remains passive: interrupted work stays suspended until Resume or Stop.
func (a *app) restoreModels(ctx context.Context) error {
	q := sam.ListQuery{}
	for {
		page, err := a.rt.ListSessions(ctx, q)
		if err != nil {
			return err
		}
		for _, session := range page.Sessions {
			settings, err := a.rt.GetConfig(ctx, session.ID)
			if err != nil {
				return err
			}
			alias := settings.Effective.Model
			model := restoredModel(session, settings)
			if _, ok := a.cfg.Providers[model.Provider]; !ok {
				return fmt.Errorf("saved provider %q is not configured; session history has not been removed", model.Provider)
			}
			a.cfg.Models[alias] = model
		}
		if page.Next == "" {
			return nil
		}
		q.After = page.Next
	}
}

func disabledTools(enabled bool) []string {
	if enabled {
		return []string{}
	}
	return []string{"read", "write", "edit", "bash", "history", "subagents"}
}

func chatModel(provider, id string, reportedContext int) sam.Model {
	// Initial local budget, NOT an upstream capability claim. The model
	// configuration editor can change this and declare verified options.
	contextTokens := 16000
	if reportedContext != 0 {
		contextTokens = min(contextTokens, reportedContext)
	}
	return sam.Model{Provider: provider, ModelID: id, ContextTokens: contextTokens}
}

func (a *app) close() {
	// Runtime must release every request owner before its AuthManager closes.
	if err := a.rt.Close(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "Close chat runtime:", err)
	}
	if err := a.auth.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "Close auth manager:", err)
	}
}
