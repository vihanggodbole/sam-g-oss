package server

import (
	"context"
	"net/http"

	sam "github.com/vihanggodbole/sam-g-oss"
)

// CatalogHost is optional host-owned configuration persistence. The stock
// executable supplies it; embedded servers without it remain discovery-only.
// Discovery snapshots are installation proposals, not a background catalog cache.
type CatalogHost interface {
	Discover(context.Context, string, string) (CatalogSnapshot, error)
	Install(context.Context, InstallCatalogRequest) (InstallCatalogResult, error)
}

type CatalogSnapshot struct {
	Models     []sam.DiscoveredModel `json:"models"`
	SnapshotID string                `json:"snapshot_id,omitempty"`
}

type InstallCatalogRequest struct {
	Provider   string `json:"-"`
	Account    string `json:"account"`
	SnapshotID string `json:"snapshot_id"`
	// On the stock host, zero uses each model's reported capacity, falling back
	// to 128,000 tokens when unavailable.
	ContextTokens int  `json:"context_tokens,omitempty"`
	ConfirmEmpty  bool `json:"confirm_empty"`
}

type InstallCatalogResult struct {
	Installed int `json:"installed"`
	Usable    int `json:"usable"`
	Removed   int `json:"removed"`
}

func (s *Server) discoverCatalog(r *http.Request) (int, any, error) {
	provider, account := r.PathValue("provider"), r.URL.Query().Get("account")
	if s.Catalogs != nil {
		out, err := s.Catalogs.Discover(r.Context(), provider, account)
		return http.StatusOK, out, err
	}
	out, err := s.rt.DiscoverModels(r.Context(), provider, account)
	return http.StatusOK, CatalogSnapshot{Models: out}, err
}

func (s *Server) installCatalog(r *http.Request) (int, any, error) {
	if s.Catalogs == nil {
		return 0, nil, &sam.Error{Code: sam.CodeUnsupported, Message: "this host does not support saved model installation"}
	}
	var req InstallCatalogRequest
	if err := s.decode(r, &req, false); err != nil {
		return 0, nil, err
	}
	req.Provider = r.PathValue("provider")
	out, err := s.Catalogs.Install(r.Context(), req)
	return http.StatusOK, out, err
}

func (s *Server) switchModel(r *http.Request) (int, any, error) {
	var req sam.SwitchModelRequest
	if err := s.decode(r, &req, false); err != nil {
		return 0, nil, err
	}
	req.RequestID, req.SessionID = requestID(r), r.PathValue("id")
	out, err := s.rt.SwitchModel(r.Context(), req)
	return http.StatusOK, out, err
}
