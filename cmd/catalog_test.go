package main

import (
	"encoding/json"
	"testing"

	sam "github.com/vihanggodbole/sam-g-oss"
	"github.com/vihanggodbole/sam-g-oss/server"
)

func TestCatalogRetainsOnlyMatchingResponsesSummaryDeclarations(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*sam.Model, *sam.DiscoveredModel)
		want   bool
	}{
		{"unknown", func(*sam.Model, *sam.DiscoveredModel) {}, true},
		{"legacy Responses", func(m *sam.Model, _ *sam.DiscoveredModel) { m.API = "" }, true},
		{"explicit false", func(_ *sam.Model, d *sam.DiscoveredModel) { d.ReasoningSummaries = new(false) }, false},
		{"explicit true", func(m *sam.Model, d *sam.DiscoveredModel) {
			m.ReasoningSummaries = false
			d.ReasoningSummaries = new(true)
		}, true},
		{"no prior declaration", func(m *sam.Model, _ *sam.DiscoveredModel) { m.ReasoningSummaries = false }, false},
		{"new ID", func(_ *sam.Model, d *sam.DiscoveredModel) { d.ID = "new-model" }, false},
		{"other provider", func(m *sam.Model, _ *sam.DiscoveredModel) { m.Provider = "other" }, false},
		{"other account", func(m *sam.Model, _ *sam.DiscoveredModel) { m.CatalogAccount = new("other") }, false},
		{"manual entry", func(m *sam.Model, _ *sam.DiscoveredModel) { m.CatalogAccount = nil }, false},
		{"Messages to Responses", func(m *sam.Model, d *sam.DiscoveredModel) {
			m.ModelID, d.ID = "claude-opus-5.5", "claude-opus-5.5"
			m.API = sam.APIAnthropicMessages
		}, false},
		{"unsupported transport", func(_ *sam.Model, d *sam.DiscoveredModel) {
			d.API = ""
			d.SupportsResponses = new(false)
			d.SupportsMessages = new(true)
		}, false},
		{"Opus profile with unknown summary fact", func(m *sam.Model, d *sam.DiscoveredModel) {
			m.ModelID, d.ID = "claude-opus-5.5", "claude-opus-5.5"
			m.ReasoningSummaries = false
			d.API = sam.APIAnthropicMessages
			d.SupportsResponses = new(false)
			d.SupportsMessages = new(true)
			d.MaxOutputTokens = new(64000)
		}, true},
		{"Opus profile with false Responses summary fact", func(_ *sam.Model, d *sam.DiscoveredModel) {
			d.ID = "claude-opus-5.5"
			d.API = sam.APIAnthropicMessages
			d.SupportsResponses = new(false)
			d.SupportsMessages = new(true)
			d.MaxOutputTokens = new(64000)
			d.ReasoningSummaries = new(false)
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := server.InstallCatalogRequest{Provider: "copilot", Account: "work"}
			previous := sam.Model{Provider: req.Provider, CatalogAccount: new(req.Account), ModelID: "gpt-6-luna", API: sam.APIResponses, ContextTokens: 128000, ReasoningSummaries: true}
			d := sam.DiscoveredModel{ID: previous.ModelID, API: sam.APIResponses, SupportsResponses: new(true)}
			test.change(&previous, &d)
			oldAlias := "manual"
			if previous.CatalogAccount != nil {
				oldAlias = catalogAlias(previous.Provider, *previous.CatalogAccount, previous.ModelID)
			}
			file := fileConfig{Config: sam.Config{Models: map[string]sam.Model{oldAlias: previous}}}
			var err error
			file.raw, err = json.Marshal(file)
			if err != nil {
				t.Fatal(err)
			}
			candidate, _, raw, err := catalogCandidate(file, req, []sam.DiscoveredModel{d})
			if err != nil {
				t.Fatal(err)
			}
			alias := catalogAlias(req.Provider, req.Account, d.ID)
			if got := candidate.Models[alias].ReasoningSummaries; got != test.want {
				t.Fatalf("published summary support = %v, want %v", got, test.want)
			}
			var saved fileConfig
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			if got := saved.Models[alias].ReasoningSummaries; got != test.want {
				t.Fatalf("saved summary support = %v, want %v", got, test.want)
			}
		})
	}
}

func TestInstalledMessagesModelRequiresReportedOutputBound(t *testing.T) {
	d := sam.DiscoveredModel{ID: "claude-opus-5.5", API: sam.APIAnthropicMessages, SupportsResponses: new(false), SupportsMessages: new(true), MaxOutputTokens: new(64000), Availability: "available", ReasoningEfforts: []string{"low", "high"}}
	req := server.InstallCatalogRequest{Provider: "copilot", Account: "personal"}
	m := installedModel(req, d)
	if m.API != sam.APIAnthropicMessages || m.MaxOutputTokens != 64000 || m.Unavailable != "" || !m.ReasoningSummaries || m.ContextTokens != fallbackCatalogContextTokens {
		t.Fatal(m)
	}
	d.MaxOutputTokens = nil
	if m = installedModel(req, d); m.Unavailable == "" || m.MaxOutputTokens != 0 {
		t.Fatal("advisory maximum became executable", m)
	}
	d.MaxOutputTokens = new(64000)
	d.Availability = "requires_action"
	if m = installedModel(req, d); m.Unavailable != "requires_action" {
		t.Fatal("policy denial concealed", m)
	}
	d.API = ""
	d.ID = "claude-haiku-4.5"
	d.Availability = "available"
	if m = installedModel(req, d); m.Unavailable == "" {
		t.Fatal("older budget-thinking model admitted", m)
	}
}
