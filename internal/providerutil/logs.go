package providerutil

import (
	"fmt"

	provider "github.com/colony-2/pulse-provider-api"
)

const MaxLogPageToken = 16384

func NormalizeLogRequest(q provider.LaunchLogRequest) (provider.LaunchLogRequest, error) {
	if q.PageSize == 0 {
		q.PageSize = 100
	}
	if !launchID.MatchString(q.LaunchID) || q.PageSize < 1 || q.PageSize > 100 || len(q.PageToken) > MaxLogPageToken {
		return q, provider.ErrLaunchLogInvalidRequest
	}
	return q, nil
}

func ValidLogRecord(r provider.LaunchLogRecord) bool {
	year := r.Timestamp.UTC().Year()
	return r.Sequence > 0 && year >= 1 && year <= 9999 &&
		r.Source.Prefix() != "" && len(r.Data) <= provider.MaxLogRecordBytes
}

func ValidateLogPage(q provider.LaunchLogRequest, page provider.LaunchLogResponse) error {
	if page.Records == nil || len(page.Records) > q.PageSize || len(page.NextPageToken) == 0 ||
		len(page.NextPageToken) > MaxLogPageToken || (len(page.Records) == 0 && page.HasMore) ||
		(len(page.Records) > 0 && page.NextPageToken == q.PageToken) ||
		(page.EstimatedTotalSizeBytes != nil && *page.EstimatedTotalSizeBytes < 0) {
		return fmt.Errorf("invalid provider launch log page")
	}
	var previous uint64
	for _, record := range page.Records {
		if !ValidLogRecord(record) || record.Sequence <= previous ||
			(previous != 0 && record.Sequence != previous+1 && !page.Incomplete) {
			return fmt.Errorf("invalid provider launch log record")
		}
		previous = record.Sequence
	}
	return nil
}
