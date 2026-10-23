// Command sam-g runs the sam agent runtime behind a small HTTP/SSE API.
// It stays in the foreground; use systemd or launchd to supervise it.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	sam "github.com/vihanggodbole/sam-g-oss"
	"github.com/vihanggodbole/sam-g-oss/internal/hostpath"
	"github.com/vihanggodbole/sam-g-oss/server"
)

const (
	defaultListen = "127.0.0.1:8080"
	shutdownGrace = 10 * time.Second
)

// Tailscale assigns node addresses from these ranges.
var tailnetPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// serverConfig holds the server-only settings stored under "server" in the
// same JSON file as the library configuration. Zero values mean defaults.
type serverConfig struct {
	Listen                 string   `json:"listen"`
	ResumeOnStart          bool     `json:"resume_on_start"`
	MaxBodyBytes           int64    `json:"max_body_bytes"`
	SSEWriteTimeoutSeconds int      `json:"sse_write_timeout_seconds"`
	AllowedOrigins         []string `json:"allowed_origins,omitempty"`
}

// fileConfig is the host file, read at startup and on SIGHUP: the library
// configuration plus the "server" section and the optional "auth" section
// for subscription logins.
type fileConfig struct {
	sam.Config
	Server serverConfig    `json:"server"`
	Auth   *sam.AuthConfig `json:"auth"`
	raw    []byte          // exact host file, including unknown fields, for atomic model edits
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		if err := authCommand(os.Args[2:], os.Stdout); err != nil {
			slog.Error("auth command failed", "error", err)
			os.Exit(1)
		}
		return
	}
	configPath := flag.String("config", "~/.sam-g/config.json", "path to the JSON configuration file")
	flag.Parse()
	if err := run(*configPath); err != nil {
		slog.Error("sam-g stopped", "error", err)
		os.Exit(1)
	}
}

func installHostDocumentation() (string, error) {
	root, err := hostpath.Resolve("~/.sam-g")
	if err != nil {
		return "", fmt.Errorf("resolve documentation directory: %w", err)
	}
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("documentation parent %q must be a real directory, not a symlink or file", root)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect documentation parent: %w", err)
	}
	dir, err := sam.InstallDocumentation(filepath.Join(root, "docs"))
	if err != nil {
		return "", fmt.Errorf("install bundled documentation: %w", err)
	}
	return dir, nil
}

func run(configPath string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	file, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	file.Config.DocumentationDir, err = installHostDocumentation()
	if err != nil {
		return err
	}
	cfg, scfg := file.Config, file.Server
	if file.Auth != nil {
		auth, err := sam.OpenAuth(*file.Auth)
		if err != nil {
			return fmt.Errorf("open auth database: %w", err)
		}
		defer auth.Close() // after the runtime, which uses it, has closed
		cfg.Auth = auth
	}
	rt, err := sam.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open runtime: %w", err)
	}
	ln, err := net.Listen("tcp", scfg.Listen)
	if err != nil {
		_ = rt.Close(context.Background())
		return err
	}
	if scfg.ResumeOnStart {
		slog.Warn(sam.DuplicateEffectWarning)
		if err := rt.Resume(ctx); err != nil {
			_ = ln.Close()
			_ = rt.Close(context.Background())
			return fmt.Errorf("resume runtime: %w", err)
		}
	}
	catalogs, err := newCatalogHost(configPath, rt, file)
	if err != nil {
		_ = ln.Close()
		_ = rt.Close(context.Background())
		return err
	}
	h := server.New(rt)
	h.Catalogs = catalogs
	if err := h.SetAllowedOrigins(scfg.AllowedOrigins); err != nil {
		_ = ln.Close()
		_ = rt.Close(context.Background())
		return err
	}
	h.MaxBodyBytes = cmp.Or(scfg.MaxBodyBytes, h.MaxBodyBytes)
	if scfg.SSEWriteTimeoutSeconds > 0 {
		h.WriteTimeout = time.Duration(scfg.SSEWriteTimeoutSeconds) * time.Second
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	slog.Info("sam-g listening", "addr", ln.Addr().String(), "database", cfg.DatabasePath)

	var reloadDone chan error
serve:
	for {
		select {
		case <-hup:
			if reloadDone != nil {
				slog.Warn("configuration reload already in progress; ignoring SIGHUP")
				continue
			}
			reloadDone = make(chan error, 1)
			// A filesystem read can stall indefinitely. Keep signal handling
			// responsive; shutdown must not wait for this host-owned task.
			go func(done chan<- error) {
				done <- catalogs.reload(ctx)
			}(reloadDone)
		case reloadErr := <-reloadDone:
			reloadDone = nil
			if reloadErr != nil {
				slog.Error("configuration reload rejected; previous configuration retained", "error", reloadErr)
			} else {
				slog.Info("configuration reloaded")
			}
		case <-ctx.Done():
			stop() // restore default handling before announcing shutdown
			slog.Info("shutdown requested; stopping new work and suspending active operations", "grace", shutdownGrace)
			break serve
		case err = <-serveErr:
			slog.Error("http server failed; shutting down", "error", err)
			break serve
		}
	}
	stop() // a second signal now terminates immediately
	if serr := shutdown(srv, rt); serr != nil {
		return serr
	}
	return err
}

// shutdown closes the listener, lets the runtime interrupt and settle active
// work (which also ends event streams), and waits for HTTP handlers, all
// within one grace period.
func shutdown(srv *http.Server, rt *sam.Runtime) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	httpDone := make(chan error, 1)
	go func() { httpDone <- srv.Shutdown(ctx) }()
	closeErr := rt.Close(ctx)
	httpErr := <-httpDone
	if closeErr != nil {
		_ = srv.Close()
		return fmt.Errorf("incomplete shutdown: %w; exiting now releases the database. Unfinished operations are reconciled on the next start, "+
			"and background processes still in their stop grace may keep running", closeErr)
	}
	if httpErr != nil {
		_ = srv.Close()
		return fmt.Errorf("incomplete shutdown: runtime closed, but HTTP connections did not finish: %w", httpErr)
	}
	slog.Info("shutdown complete")
	return nil
}

func loadConfig(path string) (fileConfig, error) {
	var file fileConfig
	path, err := hostpath.Resolve(path)
	if err != nil {
		return file, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return file, fmt.Errorf("parse %s: %w", path, err)
	}
	file.raw = raw
	file.DatabasePath, err = hostpath.Resolve(cmp.Or(file.DatabasePath, "~/.sam-g/sessions.db"))
	if err != nil {
		return file, err
	}
	if file.Auth != nil {
		file.Auth.DatabasePath, err = hostpath.Resolve(cmp.Or(file.Auth.DatabasePath, "~/.sam-g/auth/auth.db"))
		if err != nil {
			return file, err
		}
	}
	if file.Server.Listen == "" {
		file.Server.Listen = defaultListen
	}
	if file.Server.MaxBodyBytes < 0 || file.Server.SSEWriteTimeoutSeconds < 0 {
		return file, fmt.Errorf("%s: server.max_body_bytes and server.sse_write_timeout_seconds must not be negative (0 means the default)", path)
	}
	return file, checkListen(file.Server.Listen)
}

// checkListen allows only loopback or Tailscale addresses, so the
// unauthenticated API is never exposed on a public interface.
func checkListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("server.listen %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("server.listen %q: host must be a loopback or Tailscale IP address", addr)
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return nil
	}
	for _, p := range tailnetPrefixes {
		if p.Contains(ip) {
			return nil
		}
	}
	return errors.New("server.listen " + addr + ": refusing to listen on a non-loopback, non-Tailscale address; " +
		"the API has no authentication. Use 127.0.0.1 (with an SSH tunnel) or this machine's Tailscale IP")
}
