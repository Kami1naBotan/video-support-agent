package eventbus

import (
	"context"
	"errors"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
)

var ErrLagUnavailable = errors.New("consumer lag unavailable")

type PartitionLag struct {
	Partition int32  `json:"partition"`
	Start     int64  `json:"start_offset"`
	End       int64  `json:"end_offset"`
	Committed *int64 `json:"committed_offset"`
	Lag       *int64 `json:"lag"`
	Status    string `json:"status"`
}

func (c *Client) SampleLag(ctx context.Context, group, topic string) ([]PartitionLag, error) {
	if c == nil || c.raw == nil ||
		!consumerGroupPattern.MatchString(group) ||
		(topic != TopicDocumentEvents && topic != TopicIndexRetry) {
		return nil, ErrLagUnavailable
	}

	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	admin := kadm.NewClient(c.raw)

	committed, err := admin.FetchOffsetsForTopics(bounded, group, topic)
	if err != nil {
		return nil, ErrLagUnavailable
	}

	starts, err := admin.ListStartOffsets(bounded, topic)
	if err != nil || starts.Error() != nil {
		return nil, ErrLagUnavailable
	}

	ends, err := admin.ListEndOffsets(bounded, topic)
	if err != nil || ends.Error() != nil {
		return nil, ErrLagUnavailable
	}

	if len(committed[topic]) != 3 || len(starts[topic]) != 3 || len(ends[topic]) != 3 {
		return nil, ErrLagUnavailable
	}

	rows := make([]PartitionLag, 0, 3)

	for partition := int32(0); partition < 3; partition++ {
		start, startOK := starts.Lookup(topic, partition)
		end, endOK := ends.Lookup(topic, partition)
		offset, commitOK := committed.Lookup(topic, partition)

		if !startOK || !endOK || !commitOK ||
			start.Err != nil || end.Err != nil || offset.Err != nil ||
			start.Offset < 0 || end.Offset < start.Offset ||
			offset.At < -1 {
			return nil, ErrLagUnavailable
		}

		row := PartitionLag{
			Partition: partition,
			Start:     start.Offset,
			End:       end.Offset,
			Status:    "uncommitted",
		}

		if offset.At >= 0 {
			value := offset.At
			row.Committed = &value

			if value < start.Offset || value > end.Offset {
				row.Status = "offset_out_of_range"
			} else {
				lag := end.Offset - value
				row.Lag = &lag
				row.Status = "available"
			}
		}

		rows = append(rows, row)
	}

	if bounded.Err() != nil {
		return nil, ErrLagUnavailable
	}
	return rows, nil
}
