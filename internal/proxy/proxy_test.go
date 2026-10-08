package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWrapBodyDailySniffGatedOn429 pins the DM-7 corpus decision on the
// proxy sniff site: Result.DailyLimit may only fire behind a 429 (the
// plugin gates the same match at a416790 lib/index.js:1579, regex at
// :1583). router.OnResult checks DailyLimit before the 2xx success case,
// so an ungated sniff would mark the daily window exhausted and rotate
// the egress on a successful stream that merely quotes a class name in
// its first peekLimit bytes.
func TestWrapBodyDailySniffGatedOn429(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "200 stream quoting a quota class",
			status: 200,
			body:   "data: {\"choices\":[{\"delta\":{\"content\":\"FreeUsageLimitError means the daily cap\"}}]}\n\ndata: [DONE]\n",
			want:   false,
		},
		{
			name:   "500 prose quoting a quota class",
			status: 500,
			body:   "upstream said GoUsageLimitError while failing",
			want:   false,
		},
		{
			name:   "429 with a quota class",
			status: 429,
			body:   `{"type":"error","error":{"type":"FreeUsageLimitError","message":"daily usage limit exceeded"}}`,
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Result
			fired := 0
			s, err := New(Config{
				OnResult: func(r Result) {
					got = r
					fired++
				},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, "/zen/v1/chat/completions", nil)
			resp := &http.Response{
				StatusCode: tc.status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}
			s.wrapBody(req, EgressDirect, resp)
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatalf("consume body: %v", err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatalf("close body: %v", err)
			}

			if fired != 1 {
				t.Fatalf("OnResult fired %d times, want 1", fired)
			}
			if got.DailyLimit != tc.want {
				t.Errorf("DailyLimit = %v, want %v (status %d)", got.DailyLimit, tc.want, tc.status)
			}
			if got.Status != tc.status {
				t.Errorf("Status = %d, want %d", got.Status, tc.status)
			}
		})
	}
}
