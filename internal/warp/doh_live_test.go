package warp

import (
	"context"
	"testing"
	"time"
)

// Live check: resolves opencode.ai through Cloudflare DoH (direct egress).
// Skipped with -short to keep unit runs hermetic.
func TestDoHResolveLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live DoH test")
	}
	r := NewResolver(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ips, err := r.LookupIP(ctx, "opencode.ai")
	if err != nil {
		t.Fatalf("lookup failed: %v", err)
	}
	if len(ips) == 0 {
		t.Fatal("no IPs")
	}
	t.Logf("opencode.ai -> %v", ips)
}
