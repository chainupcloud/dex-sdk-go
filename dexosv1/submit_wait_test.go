package dexosv1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func waitMatchServer(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/commands" {
			t.Errorf("path %s", r.URL.Path)
		}
		query = r.URL.RawQuery
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL), &query
}

func waitMatchCommand() Command {
	var id Hash
	id[0] = 0xab
	return Command{Bytes: []byte{1}, ActionID: id}
}

func TestSubmitCommandWaitMatchReturnsTheMatchReceipt(t *testing.T) {
	cmd := waitMatchCommand()
	body := fmt.Sprintf(`{"status":"Executed","requestId":"%s","seq":"77","sub":2,"duplicate":false,"outcome":"Succeeded",
		"businessRejected":null,"stage":"matched","events":[{"kind":"OrderPlaced","data":{}}],"items":[{"status":"accepted"}]}`, cmd.ActionID.Hex())
	c, query := waitMatchServer(t, http.StatusOK, body)
	ack, err := c.SubmitCommand(context.Background(), cmd, WaitMatch())
	if err != nil {
		t.Fatal(err)
	}
	if *query != "wait=match" {
		t.Fatalf("query %q, want wait=match", *query)
	}
	if ack.Duplicate || ack.Receipt == nil {
		t.Fatalf("a fresh submission answered at match is not a duplicate and carries its receipt: %+v", ack)
	}
	r := ack.Receipt
	if ack.Seq != 77 || ack.Sub != 2 || r.Status != CommandExecuted || r.Stage != StageMatched || r.Outcome != "Succeeded" || len(r.Items) != 1 || len(r.Events) != 1 {
		t.Fatalf("ack %+v receipt %+v", ack, r)
	}
}

func TestSubmitCommandWaitMatchTimeoutLeavesTheResultPending(t *testing.T) {
	cmd := waitMatchCommand()
	body := fmt.Sprintf(`{"status":"Accepted","requestId":"%s","seq":"78","sub":0,"duplicate":false,"resultsPending":true}`, cmd.ActionID.Hex())
	c, _ := waitMatchServer(t, http.StatusAccepted, body)
	ack, err := c.SubmitCommand(context.Background(), cmd, WaitMatch())
	if err != nil {
		t.Fatal(err)
	}
	if ack.Receipt != nil || ack.Duplicate || ack.Seq != 78 {
		t.Fatalf("202 is sequenced only, the result comes from GET /v1/requests: %+v", ack)
	}
}

func TestSubmitCommandResubmissionIsADuplicateWithOrWithoutWait(t *testing.T) {
	cmd := waitMatchCommand()
	body := fmt.Sprintf(`{"status":"Executed","requestId":"%s","seq":"79","sub":1,"duplicate":true,"outcome":"Succeeded"}`, cmd.ActionID.Hex())
	for _, opts := range [][]SubmitOption{nil, {WaitMatch()}} {
		c, query := waitMatchServer(t, http.StatusOK, body)
		ack, err := c.SubmitCommand(context.Background(), cmd, opts...)
		if err != nil {
			t.Fatal(err)
		}
		if !ack.Duplicate || ack.Receipt == nil || ack.Receipt.Stage != "" || ack.Seq != 79 {
			t.Fatalf("query %q: %+v", *query, ack)
		}
	}
}

func TestSubmitCommandWaitMatchOnANonOrderCommandIsRejected(t *testing.T) {
	c, _ := waitMatchServer(t, http.StatusBadRequest, `{"error":{"code":"wait_match_unsupported_command"}}`)
	_, err := c.SubmitCommand(context.Background(), waitMatchCommand(), WaitMatch())
	var rejected *CommandRejectedError
	if !errors.As(err, &rejected) || rejected.Code != "wait_match_unsupported_command" {
		t.Fatalf("err %v", err)
	}
}
