package evaluation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestJSONLStrictContract(t *testing.T) {
	c := contractCase("jsonl")
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"no_newline": raw, "lf": append(append([]byte{}, raw...), '\n'), "crlf": append(append([]byte{}, raw...), '\r', '\n')} {
		t.Run(name, func(t *testing.T) {
			got, e := LoadCases(bytes.NewReader(data))
			if e != nil || len(got) != 1 {
				t.Fatalf("valid input rejected: %v", e)
			}
		})
	}
	base := string(raw)
	var many strings.Builder
	for i := 0; i <= MaxCases; i++ {
		many.WriteString(strings.Replace(base, `"id":"jsonl"`, fmt.Sprintf(`"id":"row-%d"`, i), 1))
		many.WriteByte('\n')
	}
	invalid := map[string]string{
		"duplicate_key":    strings.Replace(base, `"id":"jsonl"`, `"id":"jsonl","id":"jsonl"`, 1),
		"nested_duplicate": strings.Replace(base, `"filters":{}`, `"filters":{"document_key":"a","document_key":"b"}`, 1),
		"unknown":          strings.Replace(base, `"schema_version":1`, `"schema_version":1,"secret":"SENTINEL_DO_NOT_ECHO"`, 1),
		"missing":          strings.Replace(base, `"expected_tools":[],`, "", 1),
		"null_array":       strings.Replace(base, `"expected_tools":[]`, `"expected_tools":null`, 1),
		"duplicate_id":     base + "\n" + base,
		"blank":            base + "\n\n",
		"bom":              "\ufeff" + base,
		"trailing":         base + " {}",
		"utf8":             string([]byte{0xff}),
		"large_line":       strings.Repeat(" ", MaxLineBytes+1),
		"too_many":         many.String(),
		"too_deep":         strings.Repeat("[", MaxJSONDepth+2) + "0" + strings.Repeat("]", MaxJSONDepth+2),
	}
	for name, data := range invalid {
		t.Run(name, func(t *testing.T) {
			got, e := LoadCases(strings.NewReader(data))
			if e == nil || got != nil {
				t.Fatal("invalid input returned success or partial data")
			}
			if name == "too_many" && !strings.Contains(e.Error(), "too many samples") {
				t.Fatalf("wrong rejection: %v", e)
			}
			if strings.Contains(e.Error(), "SENTINEL_DO_NOT_ECHO") {
				t.Fatal("error leaked content")
			}
		})
	}
}

func TestArtifactStrictReadAndExclusiveWrite(t *testing.T) {
	type payload struct {
		Ready    bool     `json:"ready"`
		Items    []string `json:"items"`
		Decision *bool    `json:"decision"`
	}
	path := filepath.Join(t.TempDir(), "artifact.json")
	wanted := payload{Items: []string{}}
	digest, e := WriteNewJSON(path, wanted)
	if e != nil {
		t.Fatal(e)
	}
	var got payload
	actual, e := ReadStrictJSON(path, &got)
	if e != nil || actual != digest || !reflect.DeepEqual(got, wanted) {
		t.Fatalf("roundtrip: %+v %v", got, e)
	}
	before, _ := os.ReadFile(path)
	if _, e = WriteNewJSON(path, payload{Ready: true, Items: []string{}}); e == nil {
		t.Fatal("overwrote evidence")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("existing file changed")
	}
	for _, raw := range []string{`{"items":[],"decision":null}`, `{"ready":null,"items":[],"decision":null}`, `{"ready":false,"items":null,"decision":null}`, `{"ready":false,"items":[],"decision":null,"extra":1}`, `{"Ready":false,"items":[],"decision":null}`, `{"ready":false,"ready":true,"items":[],"decision":null}`, `{"ready":false,"items":[],"decision":null} {}`} {
		if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		target := payload{Ready: true, Items: []string{"unchanged"}}
		if _, e := ReadStrictJSON(path, &target); e == nil {
			t.Fatalf("accepted invalid JSON: %s", raw)
		}
		if !target.Ready || len(target.Items) != 1 || target.Items[0] != "unchanged" {
			t.Fatal("failed read modified destination")
		}
	}
}

func contractSnapshot(t *testing.T) ScoreSnapshot {
	t.Helper()
	c := contractCase("snapshot")
	digest := strings.Repeat("a", 64)
	s := ScoreSnapshot{SchemaVersion: SchemaVersion, RunID: "contract-run", Mode: "chat", RunIndex: 1, PlannedCases: 1, Identity: EvaluationIdentity{SourceSHA256: digest, InputsSHA256: digest, CorpusSHA256: digest, BindingSHA256: digest, ModelConfigSHA256: digest, PlannerModel: "synthetic", AnswerModel: "synthetic", EmbeddingModel: "synthetic"}, Cases: []Case{c}, Agents: []AgentObservation{contractObservation(c)}, Retrieval: []RetrievalObservation{}}
	frozen, e := FreezeScoreSnapshot(s)
	if e != nil {
		t.Fatal(e)
	}
	return frozen
}

func TestSnapshotIntegrityAndReviewDraft(t *testing.T) {
	s := contractSnapshot(t)
	draft, e := NewReviewDraft(s, strings.Repeat("b", 64))
	if e != nil {
		t.Fatal(e)
	}
	if len(draft.Reviews) != 1 || draft.Reviews[0].Citations == nil || draft.Reviews[0].RequiredPointsMet == nil {
		t.Fatal("draft shape invalid")
	}
	if _, e = ScoreSnapshotData(s, draft.Reviews); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"case", "observation", "count", "run", "corpus"} {
		t.Run(name, func(t *testing.T) {
			x := contractSnapshot(t)
			switch name {
			case "case":
				x.Cases[0].Question = "changed"
			case "observation":
				x.Agents[0].Answer = "changed"
			case "count":
				x.PlannedCases = 2
			case "run":
				x.Agents[0].RunID = "other"
				x.ObservationsSHA256, _ = snapshotObservationDigest(x)
			case "corpus":
				x.Identity.CorpusSHA256 = strings.Repeat("c", 64)
			}
			if _, e := ScoreSnapshotData(x, nil); e == nil {
				t.Fatal("inconsistent snapshot accepted")
			}
		})
	}
}
