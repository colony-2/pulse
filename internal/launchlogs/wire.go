// Package launchlogs captures provider observations and reads their archives.
package launchlogs

import (
	"time"

	provider "github.com/colony-2/pulse-provider-api"
)

// Record is the JSON representation shared by log pages and local files.
// Encoding a byte slice preserves non-UTF-8 output as base64.
type Record struct {
	Sequence  uint64             `json:"sequence"`
	Timestamp time.Time          `json:"timestamp"`
	Source    provider.LogSource `json:"source"`
	Data      []byte             `json:"data"`
}

func newRecord(r provider.LaunchLogRecord) Record {
	return Record{Sequence: r.Sequence, Timestamp: r.Timestamp.UTC(), Source: r.Source, Data: append([]byte{}, r.Data...)}
}

func (r Record) native() provider.LaunchLogRecord {
	return provider.LaunchLogRecord{Sequence: r.Sequence, Timestamp: r.Timestamp, Source: r.Source, Data: r.Data}
}

type Page struct {
	Records                 []Record `json:"records"`
	NextPageToken           string   `json:"next_page_token"`
	HasMore                 bool     `json:"has_more"`
	Finalized               bool     `json:"finalized"`
	Incomplete              bool     `json:"incomplete"`
	EstimatedTotalSizeBytes *int64   `json:"estimated_total_size_bytes,omitempty"`
}

func NewPage(r provider.LaunchLogResponse, token string) Page {
	p := Page{Records: make([]Record, 0, len(r.Records)), NextPageToken: token, HasMore: r.HasMore,
		Finalized: r.Finalized, Incomplete: r.Incomplete, EstimatedTotalSizeBytes: r.EstimatedTotalSizeBytes}
	for _, record := range r.Records {
		p.Records = append(p.Records, newRecord(record))
	}
	return p
}

func (p Page) Response(token string) provider.LaunchLogResponse {
	r := provider.LaunchLogResponse{NextPageToken: token, HasMore: p.HasMore, Finalized: p.Finalized,
		Incomplete: p.Incomplete, EstimatedTotalSizeBytes: p.EstimatedTotalSizeBytes}
	if p.Records != nil {
		r.Records = make([]provider.LaunchLogRecord, len(p.Records))
		for i, record := range p.Records {
			r.Records[i] = record.native()
		}
	}
	return r
}
