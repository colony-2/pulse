package launchlogs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	provider "github.com/colony-2/pulse-provider-api"
)

func TestArchiveCheckpointSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() *Store {
		t.Helper()
		store, err := Open(t.Context(), dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	const service, id = "worker", "launch"
	store := open()
	batch := store.Begin(service, []provider.Launch{{LaunchID: id}})
	listener := batch.Options().Listener
	write := func(seq uint64, data []byte) {
		t.Helper()
		err := listener.OnLog(t.Context(), provider.LaunchLog{LaunchID: id, Sequence: seq, Timestamp: time.Now(), Source: provider.LogSourceStderr, Data: data})
		if err != nil {
			t.Fatal(err)
		}
	}
	binaryOutput := []byte{0xff, '\n', 0, 1}
	write(1, binaryOutput)
	q := provider.LaunchLogRequest{LaunchID: id, PageSize: 1}
	first, err := store.Read(t.Context(), service, q)
	if err != nil || len(first.Records) != 1 || !bytes.Equal(first.Records[0].Data, binaryOutput) || first.Finalized || first.HasMore || first.Incomplete {
		t.Fatal(first, err)
	}
	write(3, []byte("after the gap"))
	backlog, err := store.Read(t.Context(), service, q)
	if err != nil || !backlog.HasMore || !backlog.Incomplete || backlog.NextPageToken != first.NextPageToken {
		t.Fatal(backlog, err)
	}
	batch.finish(nil, false)
	if info, err := os.Stat(filepath.Join(dir, fileName(scopeFor(service), id))); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = open()
	defer store.Close()
	q.PageToken = first.NextPageToken
	last, err := store.Read(t.Context(), service, q)
	if err != nil || len(last.Records) != 1 || last.Records[0].Sequence != 3 || last.HasMore || !last.Finalized || !last.Incomplete {
		t.Fatal(last, err)
	}
	q.PageToken = last.NextPageToken
	end, err := store.Read(t.Context(), service, q)
	if err != nil || len(end.Records) != 0 || end.NextPageToken != last.NextPageToken || end.HasMore {
		t.Fatal(end, err)
	}
	q.PageToken += "tampered"
	if _, err := store.Read(t.Context(), service, q); !errors.Is(err, provider.ErrLaunchLogCursor) {
		t.Fatal(err)
	}
	if _, err := store.Read(t.Context(), "other-worker", q); !errors.Is(err, provider.ErrLaunchLogNotFound) {
		t.Fatal(err)
	}
}
