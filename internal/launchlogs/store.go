package launchlogs

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	provider "github.com/colony-2/pulse-provider-api"
	"github.com/colony-2/pulse/internal/providerutil"
)

const (
	maxFileBytes = 16 << 20
	maxLineBytes = 8 << 10
)

type Store struct {
	root   *os.Root
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	watch  sync.WaitGroup
	active map[string]*capture
}

func Open(ctx context.Context, directory string, log *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	if log == nil {
		log = slog.Default()
	}
	return &Store{root: root, log: log, ctx: ctx, cancel: cancel, active: make(map[string]*capture)}, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.watch.Wait()
	return s.root.Close()
}

type fileHeader struct {
	Key []byte `json:"key"`
}

type capture struct {
	started    bool
	incomplete bool
	full       bool
	size       int
	sequence   uint64
}

type Batch struct {
	store   *Store
	service string
	scope   string
	files   map[string]*capture
	done    bool
}

func scopeFor(service string) string {
	sum := sha256.Sum256([]byte(service))
	return hex.EncodeToString(sum[:])
}

func fileName(scope, id string) string { return filepath.Join(scope, id+".jsonl") }

func (s *Store) Begin(service string, launches []provider.Launch) *Batch {
	b := &Batch{store: s, service: service, scope: scopeFor(service), files: make(map[string]*capture, len(launches))}
	for _, launch := range launches {
		b.files[launch.LaunchID] = &capture{}
	}
	return b
}

func (b *Batch) Options() provider.SubmitOptions {
	return provider.SubmitOptions{Listener: &provider.ListenOptions{Context: b.store.ctx, OnLog: b.write}}
}

func (b *Batch) write(ctx context.Context, event provider.LaunchLog) error {
	s := b.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil || ctx.Err() != nil || b.done {
		return provider.ErrStopListening
	}
	c := b.files[event.LaunchID]
	record := provider.LaunchLogRecord{Sequence: event.Sequence, Timestamp: event.Timestamp, Source: event.Source, Data: event.Data}
	if c == nil || !providerutil.ValidLogRecord(record) || record.Sequence <= c.sequence {
		return provider.ErrLogProtocol
	}
	if c.full {
		return nil
	}
	if record.Sequence != c.sequence+1 {
		c.incomplete = true
	}
	c.sequence = record.Sequence
	line, err := json.Marshal(newRecord(record))
	if err == nil {
		if c.size+len(line)+1 > maxFileBytes {
			c.incomplete, c.full = true, true
			return nil
		}
		err = b.append(event.LaunchID, c, append(line, '\n'))
	}
	if err != nil {
		c.incomplete = true
		s.log.Warn("launch log capture failed", "provider", b.service, "launch_id", event.LaunchID, "error", err)
	}
	return err
}

func (b *Batch) append(id string, c *capture, line []byte) error {
	s := b.store
	path := fileName(b.scope, id)
	if !c.started {
		if err := s.root.MkdirAll(b.scope, 0700); err != nil {
			return err
		}
		var key [32]byte
		rand.Read(key[:])
		header, err := json.Marshal(fileHeader{Key: key[:]})
		if err != nil {
			return err
		}
		file, err := s.root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(append(append(header, '\n'), line...))
		if err = errors.Join(writeErr, file.Close()); err != nil {
			return errors.Join(err, s.root.Remove(path))
		}
		c.started, c.size = true, len(line)
		s.active[path] = c
		return nil
	}
	file, err := s.root.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(line)
	if writeErr != nil && n > 0 {
		info, statErr := file.Stat()
		if statErr == nil {
			statErr = file.Truncate(info.Size() - int64(n))
		}
		writeErr = errors.Join(writeErr, statErr)
	}
	c.size += n
	return errors.Join(writeErr, file.Close())
}

// Track gives the listener the controller's lifetime rather than Submit's
// admission deadline. The provider is still responsible for admission results.
func (b *Batch) Track(subscriptions []provider.LogSubscription) {
	s := b.store
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		for _, sub := range subscriptions {
			sub.Stop()
		}
		return
	}
	s.watch.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.watch.Done()
		incomplete := make(map[string]bool)
		interrupted := len(subscriptions) == 0
		for _, sub := range subscriptions {
			result, err := sub.Wait(s.ctx)
			sub.Stop()
			if err != nil || result.Reason != provider.ListenerSourcesClosed {
				// Some providers return partial results with a method error.
				// Coverage still identifies the launches observed by this handle.
				coverage := sub.Coverage()
				interrupted = interrupted || len(coverage) == 0
				for _, source := range coverage {
					incomplete[source.LaunchID] = true
				}
			}
			for _, source := range result.Coverage {
				if source.State != provider.SourceClosed {
					incomplete[source.LaunchID] = true
				}
			}
		}
		b.finish(incomplete, interrupted || s.ctx.Err() != nil)
	}()
}

func (b *Batch) finish(incomplete map[string]bool, interrupted bool) {
	s := b.store
	s.mu.Lock()
	defer s.mu.Unlock()
	b.done = true
	for id, c := range b.files {
		if !c.started {
			continue
		}
		state := byte('0')
		if c.incomplete || incomplete[id] || interrupted {
			state = '1'
		}
		path := fileName(b.scope, id)
		if err := s.root.WriteFile(path+".done", []byte{state}, 0600); err != nil {
			s.log.Warn("launch log finalization failed", "provider", b.service, "launch_id", id, "error", err)
		}
		delete(s.active, path)
	}
}

// Read returns one page from the controller archive. File checkpoints are
// authenticated by the launch file's key, including after a controller restart.
func (s *Store) Read(ctx context.Context, service string, q provider.LaunchLogRequest) (provider.LaunchLogResponse, error) {
	q, err := providerutil.NormalizeLogRequest(q)
	if err != nil {
		return provider.LaunchLogResponse{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return provider.LaunchLogResponse{}, err
	}
	path := fileName(scopeFor(service), q.LaunchID)
	file, err := s.root.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return provider.LaunchLogResponse{}, provider.ErrLaunchLogNotFound
	}
	if err != nil {
		return provider.LaunchLogResponse{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return provider.LaunchLogResponse{}, err
	}
	reader := bufio.NewReaderSize(file, maxLineBytes)
	line, err := reader.ReadSlice('\n')
	var header fileHeader
	if err != nil || json.Unmarshal(line, &header) != nil || len(header.Key) != 32 {
		return provider.LaunchLogResponse{}, fmt.Errorf("invalid launch log file header")
	}
	start := int64(len(line))
	if q.PageToken != "" {
		start, err = checkpointOffset(q.PageToken, header.Key, start, info.Size())
		if err != nil {
			return provider.LaunchLogResponse{}, err
		}
	}
	if info.Size() > maxFileBytes+maxLineBytes {
		return provider.LaunchLogResponse{}, fmt.Errorf("invalid launch log file size")
	}
	page := provider.LaunchLogResponse{Records: make([]provider.LaunchLogRecord, 0, q.PageSize)}
	state, err := s.root.ReadFile(path + ".done")
	switch {
	case errors.Is(err, os.ErrNotExist):
		active := s.active[path]
		page.Finalized = active == nil
		page.Incomplete = page.Finalized
		if active != nil {
			page.Incomplete = active.incomplete
		}
	case err != nil:
		return provider.LaunchLogResponse{}, err
	case len(state) != 1 || (state[0] != '0' && state[0] != '1'):
		return provider.LaunchLogResponse{}, fmt.Errorf("invalid launch log finalization state")
	default:
		page.Finalized, page.Incomplete = true, state[0] == '1'
	}
	reader.Reset(io.NewSectionReader(file, start, info.Size()-start))
	position := start
	for len(page.Records) <= q.PageSize {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, io.EOF) {
			if len(line) != 0 {
				page.Incomplete = true
			}
			break
		}
		var record Record
		if err != nil || json.Unmarshal(line, &record) != nil || !providerutil.ValidLogRecord(record.native()) {
			return provider.LaunchLogResponse{}, fmt.Errorf("invalid launch log file record")
		}
		if len(page.Records) == q.PageSize {
			page.HasMore = true
			break
		}
		page.Records = append(page.Records, record.native())
		position += int64(len(line))
	}
	page.NextPageToken = checkpoint(position, header.Key)
	return page, nil
}

func checkpoint(offset int64, key []byte) string {
	position := binary.BigEndian.AppendUint64(nil, uint64(offset))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(position)
	return string(mac.Sum(position))
}

func checkpointOffset(token string, key []byte, start, end int64) (int64, error) {
	if len(token) != 8+sha256.Size {
		return 0, provider.ErrLaunchLogCursor
	}
	offset := int64(binary.BigEndian.Uint64([]byte(token[:8])))
	if offset < start || offset > end || !hmac.Equal([]byte(token), []byte(checkpoint(offset, key))) {
		return 0, provider.ErrLaunchLogCursor
	}
	return offset, nil
}
