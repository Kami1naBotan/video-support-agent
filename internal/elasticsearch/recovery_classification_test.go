package elasticsearch

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
)

func TestESProtocolAliasLookupFailureClassification(t *testing.T) {
	paths := []struct {
		name string
		path string
		call func(*Client) error
	}{
		{"published_alias", "/_alias/" + protocolAlias, func(c *Client) error {
			_, err := c.GetAliasTarget(context.Background())
			return err
		}},
		{"physical_target_guard", "/" + protocolIndex + "/_alias", func(c *Client) error {
			return c.requireUnaliasedIndex(context.Background(), protocolIndex)
		}},
	}
	for _, path := range paths {
		for _, status := range []int{400, 401, 403, 408, 429, 500, 503, 599} {
			t.Run(path.name+"/"+strconv.Itoa(status), func(t *testing.T) {
				c := protocolScript(t, protocolStep{
					method: http.MethodGet, path: path.path,
					status: status, body: protocolSecret,
				})
				want, forbidden := ErrRejected, ErrUnavailable
				if status == 408 || status == 429 || status >= 500 {
					want, forbidden = ErrUnavailable, ErrRejected
				}
				err := path.call(c)
				requireProtocolError(t, err, want)
				if errors.Is(err, forbidden) {
					t.Fatalf("contradictory failure class: %v", err)
				}
			})
		}
	}
}

func TestESProtocolMixedBulkFailureClassification(t *testing.T) {
	for _, statuses := range [][]int{{429, 503}, {429, 400}, {400, 429}} {
		t.Run(strconv.Itoa(statuses[0])+"_"+strconv.Itoa(statuses[1]), func(t *testing.T) {
			documents := protocolDocuments(t, 2, 1)
			items := protocolBulkItems(protocolIDs(documents))
			for i, status := range statuses {
				item := items[i].(map[string]any)["index"].(map[string]any)
				item["status"] = status
				item["error"] = map[string]any{"reason": protocolSecret}
			}
			c := protocolScript(t, protocolUnaliasedStep(), protocolStep{
				method: http.MethodPost, path: "/" + protocolIndex + "/_bulk",
				body: protocolJSON(t, map[string]any{"errors": true, "items": items}),
			})
			want, forbidden := ErrRejected, ErrUnavailable
			if statuses[0] == 429 && statuses[1] == 503 {
				want, forbidden = ErrUnavailable, ErrRejected
			}
			err := c.BulkIndex(context.Background(), protocolIndex, documents)
			requireProtocolError(t, err, want)
			if errors.Is(err, forbidden) {
				t.Fatalf("contradictory failure class: %v", err)
			}
		})
	}
}
