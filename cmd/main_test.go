package main

import "testing"

func TestCheckListen(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8080":              true,
		"[::1]:8080":                  true,
		"localhost:8080":              true,
		"100.101.102.103:8080":        true,
		"[fd7a:115c:a1e0::1]:8080":    true,
		"0.0.0.0:8080":                false,
		":8080":                       false,
		"192.168.1.5:8080":            false,
		"100.128.0.1:8080":            false, // outside the Tailscale range
		"example.com:8080":            false,
		"127.0.0.1":                   false, // missing port
		"[fd7a:115c:a1e1::1]:8080":    false,
		"[::ffff:127.0.0.1]:8080":     true,
		"[::ffff:100.64.0.1]:8080":    true,
		"[2001:db8::1]:8080":          false,
		"[fe80::1%en0]:8080":          false,
		"[::]:8080":                   false,
		"100.63.255.255:8080":         false,
		"100.127.255.255:8080":        true,
		"[fd7a:115c:a1e0:ffff::]:808": true,
	} {
		if err := checkListen(addr); (err == nil) != ok {
			t.Errorf("checkListen(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

func TestLoadConfigReadsExampleOnce(t *testing.T) {
	file, err := loadConfig("../docs/sam-g.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if file.Server.Listen != "127.0.0.1:8080" || len(file.Models) != 3 || file.Defaults.Compaction.Model != "summary" ||
		file.Providers["openai"].APIKeyEnv != "OPENAI_API_KEY" {
		t.Fatalf("%+v", file)
	}
}

func TestLoadSubscriptionExample(t *testing.T) {
	file, err := loadConfig("../docs/sam-g.subscription.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if file.Auth == nil || file.Auth.OpenAI == nil || file.Auth.GitHubCopilot == nil ||
		file.Providers["chatgpt"].Auth != "openai-chatgpt" || file.Defaults.Account != "personal" {
		t.Fatalf("%+v", file)
	}
}
