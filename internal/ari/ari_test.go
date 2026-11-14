package ari

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Client{
		user:     "user",
		password: "pass",
		baseURL:  server.URL,
		http:     &http.Client{Timeout: 5 * time.Second},
	}
}

func TestAddToBridgeRetriesOn422(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") != "chan1" {
			t.Errorf("channel クエリがない: %s", r.URL.RawQuery)
		}
		if calls.Add(1) < 3 {
			http.Error(w, "not in stasis", http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := client.AddToBridge(context.Background(), "br1", "chan1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestAddToBridgeDoesNotRetryOtherErrors(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if err := client.AddToBridge(context.Background(), "br1", "chan1"); err == nil {
		t.Fatal("エラーにならない")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestRequestSendsBasicAuthAndParsesChannel(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "user" || pass != "pass" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/channels/externalMedia" || r.URL.Query().Get("external_host") != "10.10.10.1:4000" {
			t.Errorf("想定外のリクエスト: %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"id":"extmedia-1","name":"UnicastRTP/10.10.10.1:4000-x",
			"channelvars":{"UNICASTRTP_LOCAL_ADDRESS":"10.10.10.10","UNICASTRTP_LOCAL_PORT":"18000"}}`))
	})
	channel, err := client.CreateExternalMedia(context.Background(), "10.10.10.1:4000", "extmedia-1")
	if err != nil {
		t.Fatal(err)
	}
	if !channel.IsExternalMedia() || channel.ChannelVars["UNICASTRTP_LOCAL_PORT"] != "18000" {
		t.Fatalf("想定外のチャネル: %+v", channel)
	}
}

func TestHangupIgnoresNotFound(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method=%s", r.Method)
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	client.Hangup(context.Background(), "gone") // パニックやハングをしないこと
}
