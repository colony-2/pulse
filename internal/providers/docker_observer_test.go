package providers_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Observe while a fixture holds the job open. Docker acknowledges both stream
// subscriptions before we release it, so even immediate failure/removal cannot
// race log collection or exit-status collection.
type dockerObservation struct {
	client     *http.Client
	path       string
	inspection json.RawMessage
	logs       chan dockerLogResult
	wait       io.ReadCloser
}

type dockerLogResult struct {
	text string
	err  error
}

func observeDocker(t *testing.T, ctx context.Context, socket, id string) *dockerObservation {
	t.Helper()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(socket, "unix://"))
	}}
	t.Cleanup(tr.CloseIdleConnections)
	o := &dockerObservation{client: &http.Client{Transport: tr}, path: "http://docker/containers/" + url.PathEscape(id)}
	request := func(method, path string) io.ReadCloser {
		req, err := http.NewRequestWithContext(ctx, method, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := o.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		if res.StatusCode != http.StatusOK {
			t.Fatalf("Docker observer %s %s: %s", method, path, res.Status)
		}
		return res.Body
	}
	inspect := request("GET", o.path+"/json")
	var err error
	o.inspection, err = io.ReadAll(inspect)
	inspect.Close()
	if err != nil {
		t.Fatal(err)
	}
	logs := request("GET", o.path+"/logs?follow=1&stdout=1&stderr=1")
	o.logs = make(chan dockerLogResult, 1)
	go func() {
		defer logs.Close()
		var output strings.Builder
		for {
			var header [8]byte
			if _, err := io.ReadFull(logs, header[:]); err != nil {
				if err == io.EOF {
					err = nil
				}
				o.logs <- dockerLogResult{strings.TrimSpace(output.String()), err}
				return
			}
			if _, err := io.CopyN(&output, logs, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
				o.logs <- dockerLogResult{output.String(), err}
				return
			}
		}
	}()
	o.wait = request("POST", o.path+"/wait?condition=removed")
	return o
}

func (o *dockerObservation) finish(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	var result struct {
		StatusCode int
		Error      *struct{ Message string }
	}
	if err := json.NewDecoder(o.wait).Decode(&result); err != nil {
		t.Fatalf("wait for Docker auto-removal: %v", err)
	}
	o.wait.Close()
	if result.Error != nil {
		t.Fatal(result.Error.Message)
	}
	logs := <-o.logs
	if logs.err != nil {
		t.Fatalf("Docker log stream: %v", logs.err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", o.path+"/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("container still exists after removal notification: %s", res.Status)
	}
	return fmt.Sprint(result.StatusCode), logs.text
}
