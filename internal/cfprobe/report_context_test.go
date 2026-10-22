package cfprobe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStoppedAgentDoesNotSendPOSTRequests(t *testing.T) {
	requests := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		_, _ = w.Write([]byte("OK"))
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := &Agent{ctx: ctx, log: newLogger(false)}
	cfg := Config{WorkerURL: server.URL}
	if _, err := a.postReportBody(cfg, []byte(`{}`), 0, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("report error=%v, want canceled", err)
	}
	if err := a.sendCorrectionConfirm(cfg, "0", "0"); !errors.Is(err, context.Canceled) {
		t.Fatalf("correction error=%v, want canceled", err)
	}
	if len(requests) != 0 {
		t.Fatal("stopped agent still sent a POST request")
	}
}
