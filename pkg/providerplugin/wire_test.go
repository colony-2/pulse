package providerplugin

import (
	"context"
	"errors"
	"fmt"
	"github.com/colony-2/pulse/pkg/compute"
	pb "github.com/colony-2/pulse/pkg/providerplugin/proto/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func launch(id string) compute.Launch {
	return compute.Launch{Request: compute.Request{LaunchID: id, Allocation: compute.Allocation{Image: "alpine:3", Platform: "linux/amd64"}, Metadata: map[string]string{"job": "λ", "custom": "雪"}}, Process: compute.Process{Command: []string{"/bin/run", "雪"}, Args: []string{"a", "b"}, Env: map[string]string{"LITERAL": "$HOME"}, WorkingDir: "/work", Stdin: "lease-secret"}}
}
func TestWireRoundTrip(t *testing.T) {
	l := launch("first")
	now := time.Date(2026, 10, 1, 1, 2, 3, 456789123, time.UTC)
	l.StartBefore = &now
	l.ImageDigest = "sha256:" + strings.Repeat("a", 64)
	l.ImageID = "native-image"
	l.Process.Stdin = compute.SecretInput(strings.Repeat("x", compute.MaxStdin))
	l.CPUMillis = compute.MaxQuantity
	l.MemoryBytes = 1
	l.ScratchBytes = 2
	l.TimeoutSeconds = 60
	in := []compute.Launch{l, launch("zero")}
	wire, e := encodeLaunches(in)
	if e != nil {
		t.Fatal(e)
	}
	data, e := proto.Marshal(wire)
	if e != nil {
		t.Fatal(e)
	}
	decoded := &pb.SubmitRequest{}
	if e = proto.Unmarshal(data, decoded); e != nil {
		t.Fatal(e)
	}
	out, e := decodeLaunches(decoded)
	if e != nil || !reflect.DeepEqual(in, out) {
		t.Fatal("round trip changed a launch", e)
	}
	q := compute.ListRequest{PageSize: 2, LaunchID: "first"}
	items := compute.ListResponse{Items: []compute.Instance{{ID: "a", LaunchID: "first", State: "running", Metadata: l.Metadata, Refs: []string{}, CreatedAt: &now}, {ID: "b", LaunchID: "first", State: "queued", Metadata: l.Metadata, Refs: []string{"native:b"}, StartedAt: &now}}, NextPageToken: "opaque"}
	w, e := encodeList(q, items)
	if e != nil {
		t.Fatal(e)
	}
	got, e := decodeList(q, w)
	if e != nil || !reflect.DeepEqual(items, got) {
		t.Fatal("list round trip", e)
	}
	empty, e := decodeList(q, &pb.ListResponse{NextPageToken: "next"})
	if e != nil || empty.Items == nil || empty.NextPageToken != "next" {
		t.Fatal(empty, e)
	}
}
func TestMalformedWire(t *testing.T) {
	for _, mutate := range []func(*pb.Launch){func(l *pb.Launch) { l.StartBefore = &timestamppb.Timestamp{Nanos: -1} }, func(l *pb.Launch) { l.CpuMillis = -1 }, func(l *pb.Launch) { l.Process.Stdin = []byte{0} }, func(l *pb.Launch) { l.Process.Stdin = []byte{255} }, func(l *pb.Launch) { l.Process.Stdin = make([]byte, compute.MaxStdin+1) }, func(l *pb.Launch) { l.Process = nil }} {
		w, _ := encodeLaunches([]compute.Launch{launch("a")})
		mutate(w.Items[0])
		if _, e := decodeLaunches(w); e == nil {
			t.Fatal("accepted invalid launch")
		}
	}
	for _, r := range []*pb.ListResponse{{Items: []*pb.Instance{{Id: "a", LaunchId: "a", State: "terminated"}}}, {NextPageToken: strings.Repeat("x", 16385)}, {Items: []*pb.Instance{nil}}} {
		if _, e := decodeList(compute.ListRequest{PageSize: 1}, r); e == nil {
			t.Fatal("accepted invalid list")
		}
	}
}
func TestIndependentOutcomes(t *testing.T) {
	ls := []compute.Launch{launch("a"), launch("b"), launch("c"), launch("d"), launch("e"), launch("f")}
	out, bad := normalizeResults(ls, []*pb.Submission{{LaunchId: "a", Status: pb.Status_ACCEPTED}, {LaunchId: "b", Status: pb.Status_NO_CAPACITY, Reason: "full"}, {LaunchId: "c", Status: pb.Status_ACCEPTED}, {LaunchId: "c", Status: pb.Status_UNAVAILABLE, Reason: "offline"}, {LaunchId: "d", Status: pb.Status(99)}, {LaunchId: "e", Status: pb.Status_ACCEPTED, Reason: strings.Repeat("x", 4097)}, {LaunchId: "unsolicited", Status: pb.Status_ACCEPTED}})
	if !bad {
		t.Fatal("missing diagnostic")
	}
	for i, want := range []compute.Status{compute.Accepted, compute.NoCapacity, compute.Unknown, compute.Unknown, compute.Unknown, compute.Unknown} {
		if out[i].Status != want {
			t.Fatal(out)
		}
	}
}

type partialProvider struct{}

func (partialProvider) Submit(_ context.Context, ls []compute.Launch) ([]compute.Submission, error) {
	return []compute.Submission{{LaunchID: ls[0].LaunchID, Status: compute.Accepted}}, errors.New("SECRET")
}
func (partialProvider) List(context.Context, compute.ListRequest) (compute.ListResponse, error) {
	return compute.ListResponse{}, errors.New("SECRET")
}
func TestServerInitializationAndPartialError(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &server{life: life, cancel: cancel, options: ServeOptions{MaxMessageBytes: DefaultMaxMessageBytes}, factory: func(ctx context.Context, i Init) (compute.Provider, error) { return partialProvider{}, nil }}
	req, _ := encodeLaunches([]compute.Launch{launch("a"), launch("b")})
	if _, e := s.Submit(life, req); e == nil {
		t.Fatal("unconfigured submission")
	}
	init := &pb.ConfigureRequest{ConfigYaml: []byte("{}"), BaseDir: t.TempDir()}
	if _, e := s.Configure(life, init); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Configure(life, init); e == nil {
		t.Fatal("reconfigured")
	}
	r, e := s.Submit(life, req)
	if e != nil || r.Results[0].Status != pb.Status_ACCEPTED || r.Results[1].Status != pb.Status_UNKNOWN || strings.Contains(r.String(), "SECRET") {
		t.Fatal("lost partial acceptance or leaked error")
	}
	if _, e = s.List(life, &pb.ListRequest{}); e == nil || strings.Contains(e.Error(), "SECRET") {
		t.Fatal(e)
	}
}
func TestStrictYAML(t *testing.T) {
	for _, s := range []string{"a: 1\na: 2", "a: &a 1\nb: *a", "[]", "a: 1\n---\na: 2", "true: 1", strings.Repeat("x", MaxConfigBytes+1)} {
		if ValidateYAML([]byte(s)) == nil {
			t.Fatal("accepted invalid YAML")
		}
	}
	var c struct {
		Text  string `yaml:"text"`
		Flag  bool   `yaml:"flag"`
		Count int    `yaml:"count"`
	}
	if e := DecodeConfig([]byte("text: '$HOME'\nflag: true\ncount: 3"), &c); e != nil || c.Text != "$HOME" || !c.Flag || c.Count != 3 {
		t.Fatal(c, e)
	}
	for _, s := range []string{"password: CANARY", "count: CANARY", "text: 12", "text: true", "flag: 1", "count: '12'"} {
		if e := DecodeConfig([]byte(s), &c); e == nil || strings.Contains(e.Error(), "CANARY") {
			t.Fatal(e)
		}
	}
}

// BenchmarkDefaultConcurrentPayload measures aggregate allocation at the shipped
// concurrency with 15 maximum-size stdin values per near-limit message.
func BenchmarkDefaultConcurrentPayload(b *testing.B) {
	ls := make([]compute.Launch, 15)
	stdin := compute.SecretInput(strings.Repeat("x", compute.MaxStdin))
	for i := range ls {
		ls[i] = launch(fmt.Sprintf("large-%d", i))
		ls[i].Process.Stdin = stdin
	}
	b.ReportAllocs()
	b.SetBytes(4 * 15 * compute.MaxStdin)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				request, e := encodeLaunches(ls)
				if e != nil {
					b.Error(e)
					return
				}
				data, e := proto.Marshal(request)
				if e != nil {
					b.Error(e)
					return
				}
				decoded := &pb.SubmitRequest{}
				if e = proto.Unmarshal(data, decoded); e != nil {
					b.Error(e)
					return
				}
				if _, e = decodeLaunches(decoded); e != nil {
					b.Error(e)
				}
			}()
		}
		wg.Wait()
	}
}
