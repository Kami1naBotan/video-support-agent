package elasticsearch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"

	"video-support-agent/internal/retrieval"
)

func TestRAGES_Version(t *testing.T) {
	for _, version := range []uint64{0, 21, 22} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			opts := retrieval.SearchOptions{VersionID: version, Limit: 1}
			body, err := BuildSearchQuery("测试", opts, searchTestTime())
			if err != nil {
				t.Fatal(err)
			}
			root := searchObject(t, searchJSONValue(t, body))
			query := searchObject(t, root["query"])
			boolean := searchObject(t, query["bool"])
			filters := searchArray(t, boolean["filter"])

			versionTerms := 0
			for _, filter := range filters {
				object := searchObject(t, filter)
				rawTerm, exists := object["term"]
				if !exists {
					continue
				}
				term := searchObject(t, rawTerm)
				value, exists := term["version_id"]
				if !exists {
					continue
				}
				versionTerms++
				if value != fmt.Sprint(version) {
					t.Fatalf("version term=%v want=%d", value, version)
				}
			}
			wantTerms := 1
			if version == 0 {
				wantTerms = 0
			}
			if versionTerms != wantTerms {
				t.Fatalf("version terms=%d want=%d", versionTerms, wantTerms)
			}

			step := searchResponseStep(protocolJSON(
				t, searchFixtureEnvelope(searchFixtureHit("10", 2)),
			))
			step.check = func(request *http.Request) {
				actual, readErr := io.ReadAll(request.Body)
				if readErr != nil || string(actual) != string(body) {
					t.Fatal("SearchCandidates lost the version query")
				}
			}
			client := protocolScript(t, searchAliasStep(), step)
			got, err := client.SearchCandidates(
				context.Background(), "测试", opts, searchTestTime(),
			)
			if version == 22 {
				searchRequireFailure(t, got, err, ErrResponse)
				return
			}
			if err != nil || len(got) != 1 || got[0].Chunk.VersionID != 21 {
				t.Fatalf("candidates=%v error=%v", got, err)
			}
		})
	}
}
