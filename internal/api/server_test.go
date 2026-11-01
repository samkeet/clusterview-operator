package api

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStartServesAndShutsDownOnCancel(t *testing.T) {
	// Grab a free port, then release it for the server to bind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	s := &Server{Addr: addr, Reader: fake.NewClientBuilder().Build(), Log: logr.Discard()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	// The listener is bound before Start serves, but the goroutine may not have
	// run yet, so retry until it answers or the deadline passes.
	httpClient := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := httpClient.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("healthz status = %d", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never answered: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v after cancel, want nil", err)
		}
	case <-time.After(shutdownTimeout + time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

func TestStartFailsOnBusyAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	s := &Server{Addr: ln.Addr().String(), Log: logr.Discard()}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start on a busy address returned nil, want an error")
	}
}

func TestNeedLeaderElection(t *testing.T) {
	if (&Server{}).NeedLeaderElection() {
		t.Error("API server must run on every replica, not just the leader")
	}
}
