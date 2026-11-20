package dexosv1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func awaitServer(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("x-dexos-seq", "90")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL), &query
}

func TestAwaitCommandAsksTheGatewayToWaitForTheMatchWithoutADigest(t *testing.T) {
	id := Hash{0xcd}
	c, query := awaitServer(t, http.StatusOK, fmt.Sprintf(`{"requestId":"%s","source":"MatchReceipt","stage":"matched","seq":"88","sub":1,
		"status":"Executed","outcome":"Succeeded","executionKnown":true,"nonceConsumed":true,"businessRejected":null,"items":[{"state":"accepted"}],"events":[]}`, id.Hex()))
	r, err := c.AwaitCommand(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if *query != "/v1/requests/"+id.Hex()+"?wait=match" {
		t.Fatalf("query %s", *query)
	}
	if r.Status != CommandExecuted || r.Stage != StageMatched || r.Seq != 88 || r.Sub != 1 || len(r.Items) != 1 || r.ReadSeq != 90 {
		t.Fatalf("%+v", r)
	}
}

func TestAwaitCommandHandsBackWhatTheGatewayKnowsWhenThereIsNoMatchReceipt(t *testing.T) {
	id := Hash{0xce}
	c, _ := awaitServer(t, http.StatusOK, fmt.Sprintf(`{"requestId":"%s","status":"Accepted","resultsPending":true,"seq":"91","sub":0}`, id.Hex()))
	if r, err := c.AwaitCommand(context.Background(), id); err != nil || r.Status != CommandAccepted || !r.ResultsPending || r.Stage != "" {
		t.Fatalf("waited 5 s without a match: %+v %v", r, err)
	}
	c, _ = awaitServer(t, http.StatusNotFound, fmt.Sprintf(`{"requestId":"%s","status":"Unknown"}`, id.Hex()))
	if r, err := c.AwaitCommand(context.Background(), id); err != nil || r.Status != CommandUnknown {
		t.Fatalf("not seen yet: %+v %v", r, err)
	}
	c, _ = awaitServer(t, http.StatusOK, fmt.Sprintf(`{"requestId":"%s","stage":"folded","status":"AuthorizationRejected","stableError":"GrantRevoked","errorCode":12}`, id.Hex()))
	if r, err := c.AwaitCommand(context.Background(), id); err != nil || r.Status != CommandAuthorizationRejected || r.Stage != StageFolded || r.StableError != "GrantRevoked" {
		t.Fatalf("refused in translation: %+v %v", r, err)
	}
	c, _ = awaitServer(t, http.StatusBadRequest, `{"error":{"code":"unsupported_request_parameter"}}`)
	if _, err := c.AwaitCommand(context.Background(), id); err == nil || errors.Is(err, ErrCommandOutcomeUnknown) {
		t.Fatalf("a refused query is an error, not an unknown outcome: %v", err)
	}
}
