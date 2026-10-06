package docker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/pulse/pkg/compute"
)

func TestNativeStdinDeliveryAndEOF(t *testing.T) {
	attached := make(chan struct{})
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/attach") {
			if r.URL.Query().Get("stdin") != "1" || r.URL.Query().Get("stdout") != "0" || r.Header.Get("Upgrade") != "tcp" {
				t.Error("incorrect attachment")
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			// Publish attachment before the response lets the client start. The
			// /start handler can run as soon as Flush sends the upgrade response.
			close(attached)
			rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			rw.Flush()
			data, err := io.ReadAll(rw)
			if err != nil {
				t.Error(err)
			}
			received <- string(data)
		} else if strings.HasSuffix(r.URL.Path, "/start") {
			select {
			case <-attached:
			default:
				t.Error("started before attaching")
			}
			w.WriteHeader(204)
		} else {
			t.Error(r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	e := &engine{base: server.URL, client: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	input := strings.Repeat("private capability\n", 16000)
	if err := e.start(ctx, "job", compute.SecretInput(input)); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-received:
		if data != input {
			t.Fatalf("stdin truncated: got %d bytes want %d", len(data), len(input))
		}
	case <-ctx.Done():
		t.Fatal("stdin did not reach EOF")
	}
}

func TestAttachmentFailureDoesNotStartOrExposeInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/start") {
			t.Error("started without stdin")
		}
		w.WriteHeader(500)
		io.WriteString(w, `{"message":"private-capability"}`)
	}))
	defer server.Close()
	e := &engine{base: server.URL, client: server.Client()}
	if err := e.start(context.Background(), "job", "private-capability"); err == nil || strings.Contains(err.Error(), "private-capability") {
		t.Fatal("unredacted attachment error", err)
	}
}

func TestBlockedAttachmentHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	e := &engine{base: server.URL, client: server.Client()}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e.start(ctx, "job", "private-capability"); err == nil {
		t.Fatal("cancelled attachment succeeded")
	}
}
