package milvus

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"video-support-agent/internal/embedding"
)

func TestRAGMilvus_Version(t *testing.T) {
	for _, version := range []uint64{0, 20, 22} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			fixture := vsNew(t)
			fixture.query.Options.VersionID = version
			embeddingCalls := 0
			embedder := vqEmbedder(func(
				_ context.Context, texts []string,
			) (embedding.BatchResult, error) {
				embeddingCalls++
				if !reflect.DeepEqual(texts, []string{"测试"}) {
					t.Fatal("query normalization changed")
				}
				return vqBatch(), nil
			})
			searcher, err := NewSearcher(
				embedder, fixture.base.client, 5*time.Second,
			)
			if err != nil {
				t.Fatal(err)
			}

			got, err := searcher.SearchCandidates(
				mvContext(t), " 测试 ",
				fixture.query.Options, fixture.query.At,
			)
			if embeddingCalls != 1 ||
				fixture.base.calls["entities/search"] != 1 {
				t.Fatal("version filtering skipped or repeated a dependency")
			}
			if version == 22 {
				if got != nil || err == nil {
					t.Fatal("mismatching response version was accepted")
				}
				return
			}
			if err != nil || len(got) != 2 {
				t.Fatalf("candidates=%v error=%v", got, err)
			}
			for _, candidate := range got {
				if candidate.Chunk.VersionID != 20 {
					t.Fatal("response version mapping changed")
				}
			}
		})
	}
}
