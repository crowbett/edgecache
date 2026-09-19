package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/crowbett/edgecache/internal/config"
)

// TestConfigSummaryRedactsOriginPassword pins that /admin/stats' config block
// never carries the userinfo password of EDGECACHE_ORIGIN.
func TestConfigSummaryRedactsOriginPassword(t *testing.T) {
	t.Parallel()
	u, err := url.Parse("https://svc:hunter2@origin.test/base")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Origin: u, OriginHost: "override.test"}
	raw, err := json.Marshal(configSummary(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("summary leaks the origin password: %s", raw)
	}
	if !strings.Contains(string(raw), `"origin":"https://svc:xxxxx@origin.test/base"`) {
		t.Fatalf("summary should carry the redacted origin: %s", raw)
	}
	if _, ok := configSummary(cfg)["purge_token"]; ok {
		t.Fatal("summary must not carry the purge token")
	}
}
