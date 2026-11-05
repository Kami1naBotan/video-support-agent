package milvus

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWorkerCollectionNamePreflight(t *testing.T) {
	for _, tc := range []struct{ name, target, want string }{
		{"generic_prefix_rejected", "knowledge_g_0123456789abcdef0123456789abcdef", "invalid new collection name"},
		{"vector_prefix_accepted", "rule_chunks_vec_v1_0123456789abcdef0123456789abcdef", "prepared import is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			client := &Client{alias: "rule_chunks_vec"}
			_, err := client.BuildCollection(ctx, tc.target, nil)
			if !errors.Is(err, ErrInput) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want preflight error %q", err, tc.want)
			}
		})
	}
}
