package providerplugin

import (
	"github.com/colony-2/pulse/pkg/compute"
	pb "github.com/colony-2/pulse/pkg/providerplugin/proto/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"strings"
	"time"
	"unicode/utf8"
)

func timestamp(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}
func instant(t *timestamppb.Timestamp) (*time.Time, error) {
	if t == nil {
		return nil, nil
	}
	if t.CheckValid() != nil {
		return nil, ErrProtocol
	}
	v := t.AsTime()
	return &v, nil
}
func encodeLaunches(ls []compute.Launch) (*pb.SubmitRequest, error) {
	if compute.ValidateLaunches(ls) != nil {
		return nil, ErrProtocol
	}
	out := &pb.SubmitRequest{}
	for _, l := range ls {
		t := timestamp(l.StartBefore)
		if t != nil && t.CheckValid() != nil {
			return nil, ErrProtocol
		}
		out.Items = append(out.Items, &pb.Launch{LaunchId: l.LaunchID, Image: l.Image, Platform: l.Platform, CpuMillis: l.CPUMillis, MemoryBytes: l.MemoryBytes, ScratchBytes: l.ScratchBytes, ImageDigest: l.ImageDigest, ImageId: l.ImageID, TimeoutSeconds: l.TimeoutSeconds, StartBefore: t, Metadata: l.Metadata, Process: &pb.Process{Command: l.Process.Command, Args: l.Process.Args, Env: l.Process.Env, WorkingDir: l.Process.WorkingDir, Stdin: []byte(l.Process.Stdin)}})
	}
	// Marshal also rejects invalid UTF-8 in protobuf string fields.
	if _, err := proto.Marshal(out); err != nil {
		return nil, ErrProtocol
	}
	return out, nil
}
func decodeLaunches(in *pb.SubmitRequest) ([]compute.Launch, error) {
	if in == nil {
		return nil, ErrProtocol
	}
	out := make([]compute.Launch, 0, len(in.Items))
	for _, l := range in.Items {
		if l == nil || l.Process == nil {
			return nil, ErrProtocol
		}
		t, e := instant(l.StartBefore)
		if e != nil {
			return nil, e
		}
		p := l.Process
		out = append(out, compute.Launch{Request: compute.Request{LaunchID: l.LaunchId, Allocation: compute.Allocation{Image: l.Image, Platform: l.Platform, CPUMillis: l.CpuMillis, MemoryBytes: l.MemoryBytes, ScratchBytes: l.ScratchBytes, ImageDigest: l.ImageDigest, ImageID: l.ImageId}, TimeoutSeconds: l.TimeoutSeconds, StartBefore: t, Metadata: l.Metadata}, Process: compute.Process{Command: append([]string{}, p.Command...), Args: append([]string{}, p.Args...), Env: p.Env, WorkingDir: p.WorkingDir, Stdin: compute.SecretInput(p.Stdin)}})
	}
	if compute.ValidateLaunches(out) != nil {
		return nil, ErrProtocol
	}
	return out, nil
}

var statusToWire = map[compute.Status]pb.Status{compute.Accepted: pb.Status_ACCEPTED, compute.NoCapacity: pb.Status_NO_CAPACITY, compute.Unsupported: pb.Status_UNSUPPORTED, compute.Unavailable: pb.Status_UNAVAILABLE, compute.Rejected: pb.Status_REJECTED, compute.Unknown: pb.Status_UNKNOWN}

func statusFromWire(s pb.Status) compute.Status {
	for k, v := range statusToWire {
		if v == s {
			return k
		}
	}
	return ""
}
func results(ls []compute.Launch, s compute.Status, reason string) []compute.Submission {
	out := make([]compute.Submission, 0, len(ls))
	for _, l := range ls {
		out = append(out, compute.Submission{LaunchID: l.LaunchID, Status: s, Reason: reason})
	}
	return out
}

// normalizeResults preserves independent valid outcomes; duplicate requested IDs
// poison only that item. Unsolicited IDs never invalidate a known acceptance.
func normalizeResults(ls []compute.Launch, in []*pb.Submission) ([]compute.Submission, bool) {
	out := results(ls, compute.Unknown, "invalid or missing provider result")
	indices := map[string]int{}
	counts := map[string]int{}
	bad := false
	for i, l := range ls {
		indices[l.LaunchID] = i
	}
	for _, r := range in {
		if r == nil {
			bad = true
			continue
		}
		i, ok := indices[r.LaunchId]
		if !ok {
			bad = true
			continue
		}
		counts[r.LaunchId]++
		s := statusFromWire(r.Status)
		if counts[r.LaunchId] != 1 || !s.Valid() || len(r.Reason) > MaxReasonBytes || !utf8.ValidString(r.Reason) || strings.ContainsAny(r.Reason, "\x00\r\n") || (s != compute.Accepted && r.Reason == "") {
			out[i] = compute.Submission{LaunchID: r.LaunchId, Status: compute.Unknown, Reason: "invalid provider result"}
			bad = true
			continue
		}
		out[i] = compute.Submission{LaunchID: r.LaunchId, Status: s, Reason: r.Reason}
	}
	for _, l := range ls {
		if counts[l.LaunchID] != 1 {
			bad = true
		}
	}
	return out, bad
}
func encodeResults(ls []compute.Launch, in []compute.Submission) ([]*pb.Submission, bool) {
	raw := make([]*pb.Submission, 0, len(in))
	for _, r := range in {
		raw = append(raw, &pb.Submission{LaunchId: r.LaunchID, Status: statusToWire[r.Status], Reason: r.Reason})
	}
	safe, bad := normalizeResults(ls, raw)
	out := make([]*pb.Submission, 0, len(safe))
	for _, r := range safe {
		out = append(out, &pb.Submission{LaunchId: r.LaunchID, Status: statusToWire[r.Status], Reason: r.Reason})
	}
	return out, bad
}
func decodeList(q compute.ListRequest, in *pb.ListResponse) (compute.ListResponse, error) {
	out := compute.ListResponse{Items: []compute.Instance{}}
	if in == nil || len(in.Items) > q.PageSize || len(in.NextPageToken) > 16384 || (in.NextPageToken != "" && in.NextPageToken == q.PageToken) {
		return out, ErrProtocol
	}
	out.NextPageToken = in.NextPageToken
	seen := map[string]bool{}
	for _, v := range in.Items {
		if v == nil {
			return compute.ListResponse{}, ErrProtocol
		}
		created, e := instant(v.CreatedAt)
		if e != nil {
			return compute.ListResponse{}, e
		}
		started, e := instant(v.StartedAt)
		if e != nil {
			return compute.ListResponse{}, e
		}
		item := compute.Instance{ID: v.Id, LaunchID: v.LaunchId, State: v.State, Metadata: v.Metadata, Refs: append([]string{}, v.Refs...), CreatedAt: created, StartedAt: started}
		if item.Validate() != nil || seen[item.ID] || (q.LaunchID != "" && q.LaunchID != item.LaunchID) {
			return compute.ListResponse{}, ErrProtocol
		}
		seen[item.ID] = true
		out.Items = append(out.Items, item)
	}
	return out, nil
}
func encodeList(q compute.ListRequest, in compute.ListResponse) (*pb.ListResponse, error) {
	out := &pb.ListResponse{NextPageToken: in.NextPageToken}
	for _, i := range in.Items {
		out.Items = append(out.Items, &pb.Instance{Id: i.ID, LaunchId: i.LaunchID, State: i.State, Metadata: i.Metadata, Refs: i.Refs, CreatedAt: timestamp(i.CreatedAt), StartedAt: timestamp(i.StartedAt)})
	}
	if _, err := decodeList(q, out); err != nil {
		return nil, err
	}
	return out, nil
}
