package dexosv1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func submitAnswering(t *testing.T, status int, header map[string]string, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	_, err := NewClient(srv.URL).SubmitCommand(context.Background(), waitMatchCommand())
	return err
}

func TestSubmitCommandRejectedBeforeAdmissionIsADefiniteRejection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		header     map[string]string
		body       string
		code       string
		retryAfter time.Duration
	}{
		{name: "api precheck rate limit", status: http.StatusTooManyRequests, header: map[string]string{"retry-after": "1"},
			body: `{"error":{"code":"command_rate_limited","num":10429,"message":"command rate limited"},"status":"RejectedBeforeAdmission","executionKnown":false,"nonceConsumed":false}`,
			code: "command_rate_limited", retryAfter: time.Second},
		{name: "client order index unavailable", status: http.StatusServiceUnavailable,
			body: `{"error":{"code":"client_order_index_unavailable","num":10503,"message":"client order index unavailable"},"status":"RejectedBeforeAdmission","executionKnown":false,"nonceConsumed":false}`,
			code: "client_order_index_unavailable"},
		{name: "unverified bucket", status: http.StatusTooManyRequests, header: map[string]string{"retry-after": "2"},
			body: `{"error":{"code":"GatewayRateLimited","num":10429,"message":"rate limit exceeded","details":{"bucket":"unverified","weight":1,"retryAfterS":2}}}`,
			code: "GatewayRateLimited", retryAfter: 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := submitAnswering(t, tc.status, tc.header, tc.body)
			var rejected *CommandRejectedError
			if !errors.As(err, &rejected) || errors.Is(err, ErrCommandOutcomeUnknown) {
				t.Fatalf("rejected before admission must be a definite rejection, not an unknown outcome: %v", err)
			}
			if rejected.Status != tc.status || rejected.Code != tc.code || rejected.RetryAfter != tc.retryAfter {
				t.Fatalf("status %d code %q retry-after %s, want %d %q %s", rejected.Status, rejected.Code, rejected.RetryAfter, tc.status, tc.code, tc.retryAfter)
			}
		})
	}
}

func TestSubmitCommandKeepsGatewayBucketsAndUnknownOutcomes(t *testing.T) {
	err := submitAnswering(t, http.StatusTooManyRequests, map[string]string{"retry-after": "3", "ratelimit": "limit=10, remaining=0, reset=3"},
		`{"error":{"code":"GatewayRateLimited","num":10429,"message":"rate limit exceeded","details":{"bucket":"ip","weight":1,"retryAfterS":3}}}`)
	var limited *RateLimitedError
	if !errors.As(err, &limited) || limited.Bucket != "ip" || limited.RetryAfter != 3*time.Second {
		t.Fatalf("an ip-bucket 429 stays a RateLimitedError: %v", err)
	}
	for _, body := range []string{
		`{"error":{"code":"submission_outcome_unknown","num":10503,"message":"unknown"},"status":"Unknown","executionKnown":false,"nonceConsumed":null}`,
		`{"error":{"code":"GatewayUnavailable","num":10503,"message":"upstream unavailable","details":{"outcome":"unknown"}}}`,
	} {
		if err := submitAnswering(t, http.StatusServiceUnavailable, nil, body); !errors.Is(err, ErrCommandOutcomeUnknown) {
			t.Fatalf("a 503 that may have been admitted stays unknown: %v (%s)", err, body)
		}
	}
}
