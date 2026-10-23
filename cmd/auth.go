package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	sam "github.com/vihanggodbole/sam-g-oss"
)

func authCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("use auth login, accounts, logout, export or import")
	}
	switch args[0] {
	case "login", "accounts", "logout", "export", "import":
	default:
		return fmt.Errorf("unknown auth command %q; use login, accounts, logout, export or import", args[0])
	}
	flags := flag.NewFlagSet("auth "+args[0], flag.ContinueOnError)
	config := flags.String("config", "~/.sam-g/config.json", "host config (no conversation database is opened)")
	account := flags.String("account", "", "saved account label")
	provider := flags.String("provider", sam.AuthOpenAIChatGPT, "login provider")
	consent := flags.Bool("enable-plan-usage", false, "explicitly request ChatGPT plan-use consent")
	output := flags.String("out", "", "protected export bundle path")
	input := flags.String("in", "", "protected import bundle path")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	file, err := loadConfig(*config)
	if err != nil {
		return err
	}
	if file.Auth == nil {
		return errors.New("config requires an auth section")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	auth, err := sam.OpenAuth(*file.Auth)
	if err != nil {
		return err
	}
	defer auth.Close()
	var result any
	switch args[0] {
	case "accounts":
		result = auth.Accounts()
	case "login":
		result, err = localLogin(ctx, auth, *provider, *account, *consent, out)
	case "export":
		result, err = auth.ExportAccount(ctx, sam.ExportAccountRequest{RequestID: authRequestID(), Account: *account, Path: *output})
	case "import":
		result, err = auth.ImportAccount(ctx, sam.ImportAccountRequest{RequestID: authRequestID(), Account: *account, Path: *input})
	case "logout":
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		result, err = auth.Logout(bounded, sam.AuthAccountRequest{RequestID: authRequestID(), Account: *account})
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}

func authRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "local_" + hex.EncodeToString(b[:])
}

func localLogin(ctx context.Context, auth *sam.AuthManager, provider, account string, consent bool, out io.Writer) (sam.LoginStatus, error) {
	req := sam.LoginRequest{RequestID: authRequestID(), Provider: provider, Account: account, EnablePlanUsage: consent}
	var listener net.Listener
	var err error
	if provider == sam.AuthOpenAIChatGPT {
		listener, err = net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return sam.LoginStatus{}, err
		}
		defer listener.Close()
		req.RedirectURI = "http://" + listener.Addr().String() + "/auth/callback"
	}
	status, err := auth.StartLogin(ctx, req)
	if err != nil {
		return status, err
	}
	if listener != nil {
		srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: loopbackCallback(auth, status.ID, req.RedirectURI)}
		serveDone := make(chan struct{})
		go func() { defer close(serveDone); _ = srv.Serve(listener) }()
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(closeCtx)
			_ = srv.Close()
			<-serveDone
		}()
		fmt.Fprintln(out, "Open this URL in your local browser:", status.AuthorizationURL)
	} else {
		fmt.Fprintf(out, "Open %s and enter %s\n", status.VerificationURL, status.UserCode)
	}
	loginID := status.ID
	status, err = auth.WaitLogin(ctx, loginID)
	if err != nil {
		_, _ = auth.CancelLogin(sam.CancelLoginRequest{RequestID: authRequestID(), LoginID: loginID})
		return status, err
	}
	return status, nil
}

func loopbackCallback(auth *sam.AuthManager, id, redirect string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet || r.URL.Path != "/auth/callback" || r.URL.RawPath != "" {
			http.NotFound(w, r)
			return
		}
		// Deliberately ignore Host. This is a separate local browser callback, not
		// an exception to the ordinary server's browser-origin rejection.
		status, err := auth.CompleteLogin(r.Context(), sam.CompleteLoginRequest{LoginID: id, CallbackURL: redirect + "?" + r.URL.RawQuery})
		if err != nil {
			http.Error(w, "Callback rejected. Return to the local command for status.", http.StatusBadRequest)
			return
		}
		if status.State != sam.LoginCompleted {
			http.Error(w, "Sign-in did not complete. Return to the local command.", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, "Sign-in complete. You may close this page.")
	})
}
