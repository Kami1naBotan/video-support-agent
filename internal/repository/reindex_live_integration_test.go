//go:build indexexportintegration && reindexintegration

package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/model"

	"gorm.io/gorm/clause"
)

type liveCLIReport struct {
	Event        string                  `json:"event"`
	Stage        string                  `json:"stage"`
	NewIndex     string                  `json:"new_index"`
	Result       *indexing.RebuildResult `json:"result"`
	ErrorClasses []string                `json:"error_classes"`
	ExitCode     int                     `json:"exit_code"`
}

func TestReindexLive(t *testing.T) {
	require := func(ok bool, message string) {
		t.Helper()
		if !ok {
			t.Fatal(message)
		}
	}

	require(os.Getenv("RUN_REINDEX_LIVE") == "1",
		"RUN_REINDEX_LIVE=1 is required; this test does not skip")

	root := os.Getenv("REINDEX_PROJECT_ROOT")
	require(filepath.IsAbs(root), "absolute REINDEX_PROJECT_ROOT is required")

	executable := filepath.Join(root, ".scratch", "elasticsearch", "reindex.exe")
	info, err := os.Stat(executable)
	exportMySQLMust(t, err)
	require(info.Mode().IsRegular(), "CLI executable must be a regular file")

	mappingPath := filepath.Join(
		root, "deploy", "elasticsearch", "rule-chunks-v1.json",
	)
	definition, err := os.ReadFile(mappingPath)
	exportMySQLMust(t, err)

	// Exact local endpoint: never inherit an arbitrary ES_URL.
	const endpoint = "http://127.0.0.1:9200"
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(client.CloseIdleConnections)

	es := func(method, path string, body []byte, wantStatus int) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(
			ctx, method, endpoint+path, bytes.NewReader(body),
		)
		exportMySQLMust(t, err)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		response, err := client.Do(req)
		exportMySQLMust(t, err)
		defer response.Body.Close()

		data, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
		exportMySQLMust(t, err)
		require(len(data) <= 4<<20, "ES diagnostic response exceeded limit")
		if response.StatusCode != wantStatus {
			t.Fatalf("%s %s: status=%d want=%d",
				method, path, response.StatusCode, wantStatus)
		}
		return data
	}

	var health struct {
		Status   string `json:"status"`
		TimedOut bool   `json:"timed_out"`
	}
	exportMySQLMust(t, json.Unmarshal(
		es("GET", "/_cluster/health?wait_for_status=yellow&timeout=5s", nil, 200),
		&health,
	))
	require(!health.TimedOut &&
		(health.Status == "yellow" || health.Status == "green"),
		"ES is not ready")

	// This established alias is only observed, never modified by this test.
	var protectedBefore any
	exportMySQLMust(t, json.Unmarshal(
		es("GET", "/_alias/rule_chunks_current", nil, 200),
		&protectedBefore,
	))

	// Reuses UUID validation, unique database creation and real model migration.
	s := newExportMySQLSuite(t)
	s.at = time.Now().UTC().Truncate(time.Millisecond)

	suffix := strings.TrimPrefix(s.name, "es_export_it_")
	alias := "rule_chunks_it_" + suffix
	evidenceDir := filepath.Join(
		root, ".scratch", "elasticsearch", "evidence", "reindex-"+suffix,
	)
	exportMySQLMust(t, os.Mkdir(evidenceDir, 0700))
	t.Logf("TEST_ALIAS=%s EVIDENCE_DIR=%s", alias, evidenceDir)

	exportMySQLMust(t, os.WriteFile(
		filepath.Join(evidenceDir, "resources.txt"),
		[]byte(fmt.Sprintf(
			"DATABASE=%s\nSERVER_UUID=%s\nALIAS=%s\nES_URL=%s\n",
			s.name, s.uuid, alias, endpoint,
		)),
		0600,
	))
	es("GET", "/_alias/"+alias, nil, 404)

	aliasTarget := func() string {
		t.Helper()
		var result map[string]struct {
			Aliases map[string]map[string]any `json:"aliases"`
		}
		exportMySQLMust(t, json.Unmarshal(
			es("GET", "/_alias/"+alias, nil, 200), &result,
		))
		require(len(result) == 1, "test alias must have exactly one target")
		for name, entry := range result {
			options, exists := entry.Aliases[alias]
			require(exists, "test alias missing from response")
			require(len(options) == 1 &&
				options["is_write_index"] == false,
				"test alias must explicitly disable writes without filters or routing")
			return name
		}
		t.Fatal("test alias has no target")
		return ""
	}

	checkCount := func(index string, want int) {
		t.Helper()
		var result struct {
			Count  int `json:"count"`
			Shards struct {
				Total      int `json:"total"`
				Successful int `json:"successful"`
				Failed     int `json:"failed"`
			} `json:"_shards"`
		}
		exportMySQLMust(t, json.Unmarshal(
			es("GET", "/"+index+"/_count", nil, 200), &result,
		))
		require(result.Count == want, fmt.Sprintf(
			"index %s: count=%d want=%d", index, result.Count, want,
		))
		require(result.Shards.Total > 0 &&
			result.Shards.Successful == result.Shards.Total &&
			result.Shards.Failed == 0, "count returned incomplete shard results")
	}

	checkNoAliases := func(index string) {
		t.Helper()
		var result map[string]struct {
			Aliases map[string]json.RawMessage `json:"aliases"`
		}
		exportMySQLMust(t, json.Unmarshal(
			es("GET", "/"+index+"/_alias", nil, 200), &result,
		))
		entry, exists := result[index]
		require(exists && len(result) == 1 && len(entry.Aliases) == 0,
			"retained physical index must exist without aliases")
	}

	runCLI := func(label, expectedDB, expectedUUID, mapping string,
		allowEmpty bool, wantExit int, wantStage, wantState string) liveCLIReport {
		t.Helper()
		args := []string{
			"-expect-db", expectedDB,
			"-expect-server-uuid", expectedUUID,
			"-mapping", mapping,
			"-timeout", "90s",
		}
		if allowEmpty {
			args = append(args, "-allow-empty")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, args...)
		command.Dir = root
		// os/exec uses the last occurrence of duplicated environment keys.
		command.Env = append(os.Environ(),
			"DB_HOST=127.0.0.1",
			"DB_PORT=13308",
			"DB_USER=root",
			"DB_PASSWORD="+os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD"),
			"DB_NAME="+s.name,
			"ES_URL="+endpoint,
			"ES_INDEX_ALIAS="+alias,
			"ES_TIMEOUT=10s",
		)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		runErr := command.Run()

		exportMySQLMust(t, os.WriteFile(
			filepath.Join(evidenceDir, label+".jsonl"), stdout.Bytes(), 0600,
		))
		exportMySQLMust(t, os.WriteFile(
			filepath.Join(evidenceDir, label+".stderr.log"), stderr.Bytes(), 0600,
		))
		require(ctx.Err() == nil, "CLI exceeded the test process deadline")

		exitCode := 0
		if runErr != nil {
			var exitError *exec.ExitError
			if !errors.As(runErr, &exitError) {
				t.Fatalf("cannot execute CLI: %v", runErr)
			}
			exitCode = exitError.ExitCode()
		}
		require(exitCode == wantExit, fmt.Sprintf(
			"%s: CLI exit=%d want=%d; inspect evidence directory",
			label, exitCode, wantExit,
		))

		lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n"))
		require(len(lines) >= 1, "CLI did not write a final report")
		var report liveCLIReport
		exportMySQLMust(t, json.Unmarshal(lines[len(lines)-1], &report))
		require(report.Event == "finished" &&
			report.ExitCode == wantExit &&
			report.Stage == wantStage,
			"CLI final report does not match expected outcome")

		if report.NewIndex != "" {
			require(len(lines) == 2, "expected started and finished records")
			var start struct {
				Event    string `json:"event"`
				NewIndex string `json:"new_index"`
			}
			exportMySQLMust(t, json.Unmarshal(lines[0], &start))
			require(start.Event == "started" &&
				start.NewIndex == report.NewIndex,
				"started and finished resource identities differ")
			require(strings.HasPrefix(report.NewIndex, "rule_chunks_v1_"),
				"unexpected physical index name")
		}

		if wantState != "" {
			require(report.Result != nil, "CLI rebuild result missing")
			require(string(report.Result.Publication.State) == wantState,
				"unexpected publication state")
			require(report.Result.NewIndex == report.NewIndex,
				"result index differs from final report")
		}

		if wantExit == 0 {
			require(len(report.ErrorClasses) == 0,
				"successful CLI report contains error classes")
			require(report.Result.SourceDatabase == s.name &&
				report.Result.SourceServerUUID == s.uuid,
				"successful CLI report has wrong source identity")
			require(report.Result.Publication.RequestAcknowledged &&
				report.Result.Publication.ObservedTarget == report.NewIndex,
				"successful publication was not acknowledged and observed")
		}

		t.Logf("CASE=%s EXIT=%d STAGE=%s INDEX=%s STATE=%s",
			label, exitCode, report.Stage, report.NewIndex, wantState)
		return report
	}

	f := s.fixture(t)
	chunks := make([]model.KnowledgeChunk, 0, 500)
	for i := uint32(1); i <= 500; i++ {
		chunks = append(chunks, model.KnowledgeChunk{
			ArticleID:  f.article.ID,
			ChunkIndex: i,
			Content:    fmt.Sprintf("隔离验收片段 %d", i),
		})
	}
	exportMySQLMust(t, s.db.Omit(clause.Associations).
		CreateInBatches(&chunks, 100).Error)

	snapshot, err := s.export(context.Background())
	exportMySQLMust(t, err)
	require(len(snapshot.Documents) == 501, "fixture must export 501 documents")

	checkSamples := func(index string) {
		t.Helper()
		for _, position := range []int{0, 250, 500} {
			expected := snapshot.Documents[position]
			var actual struct {
				Index  string          `json:"_index"`
				ID     string          `json:"_id"`
				Found  bool            `json:"found"`
				Source json.RawMessage `json:"_source"`
			}
			exportMySQLMust(t, json.Unmarshal(
				es("GET", "/"+index+"/_doc/"+expected.ChunkID+
					"?realtime=false", nil, 200), &actual,
			))
			require(actual.Found && actual.Index == index &&
				actual.ID == expected.ChunkID, "sample document identity mismatch")

			expectedJSON, err := json.Marshal(expected)
			exportMySQLMust(t, err)
			var want, got any
			exportMySQLMust(t, json.Unmarshal(expectedJSON, &want))
			exportMySQLMust(t, json.Unmarshal(actual.Source, &got))
			require(reflect.DeepEqual(want, got),
				"sample full _source differs from MySQL projection")
		}
	}

	first := runCLI("first", s.name, s.uuid, mappingPath,
		false, 0, "complete", "applied")
	require(first.Result.DocumentCount == 501 && first.Result.OldIndex == "",
		"first build should publish 501 documents without a prior alias")
	require(aliasTarget() == first.NewIndex, "first alias target mismatch")
	checkCount(first.NewIndex, 501)
	checkSamples(first.NewIndex)

	second := runCLI("repeat", s.name, s.uuid, mappingPath,
		false, 0, "complete", "applied")
	require(second.NewIndex != first.NewIndex &&
		second.Result.OldIndex == first.NewIndex &&
		second.Result.DocumentCount == 501, "repeat build identity/count mismatch")
	require(aliasTarget() == second.NewIndex, "repeat alias target mismatch")
	checkCount(second.NewIndex, 501)
	checkSamples(second.NewIndex)
	checkCount(first.NewIndex, 501)
	checkNoAliases(first.NewIndex)

	wrongDB := runCLI("wrong-database", s.name+"_wrong", s.uuid, mappingPath,
		false, 2, "configuration", "")
	require(wrongDB.NewIndex == "" && wrongDB.Result == nil,
		"database configuration mismatch should fail before rebuild")
	require(aliasTarget() == second.NewIndex, "wrong database changed alias")

	wrongUUID := "00000000-0000-0000-0000-000000000000"
	require(s.uuid != wrongUUID, "unexpected test instance UUID")
	wrongSource := runCLI("wrong-instance", s.name, wrongUUID, mappingPath,
		false, 1, "source-check", "not_attempted")
	es("GET", "/"+wrongSource.NewIndex, nil, 404)
	require(aliasTarget() == second.NewIndex, "wrong instance changed alias")

	// Only this private mapping is narrowed; the deployed mapping is unchanged.
	var narrow map[string]any
	exportMySQLMust(t, json.Unmarshal(definition, &narrow))
	mappings, ok := narrow["mappings"].(map[string]any)
	require(ok, "mapping object missing")
	properties, ok := mappings["properties"].(map[string]any)
	require(ok, "mapping properties missing")
	properties["chunk_index"] = map[string]any{"type": "byte"}
	narrowJSON, err := json.Marshal(narrow)
	exportMySQLMust(t, err)
	narrowPath := filepath.Join(evidenceDir, "partial-failure-mapping.json")
	exportMySQLMust(t, os.WriteFile(narrowPath, narrowJSON, 0600))

	partial := runCLI("partial-bulk", s.name, s.uuid, narrowPath,
		false, 1, "bulk", "not_attempted")
	require(partial.Result.DocumentCount == 501, "partial build source count mismatch")
	require(partial.NewIndex != first.NewIndex &&
		partial.NewIndex != second.NewIndex, "partial build reused an old index")
	require(aliasTarget() == second.NewIndex, "partial Bulk failure published")

	var refresh struct {
		Shards struct {
			Total      int `json:"total"`
			Successful int `json:"successful"`
			Failed     int `json:"failed"`
		} `json:"_shards"`
	}
	exportMySQLMust(t, json.Unmarshal(
		es("POST", "/"+partial.NewIndex+"/_refresh", nil, 200), &refresh,
	))
	require(refresh.Shards.Total > 0 &&
		refresh.Shards.Successful == refresh.Shards.Total &&
		refresh.Shards.Failed == 0, "diagnostic refresh failed")
	checkCount(partial.NewIndex, 128)
	checkNoAliases(partial.NewIndex)

	// Confirm a successful item, a rejected item and the next batch's item.
	es("GET", "/"+partial.NewIndex+"/_doc/"+
		snapshot.Documents[0].ChunkID, nil, 200)
	es("GET", "/"+partial.NewIndex+"/_doc/"+
		snapshot.Documents[128].ChunkID, nil, 404)
	es("GET", "/"+partial.NewIndex+"/_doc/"+
		snapshot.Documents[500].ChunkID, nil, 404)
	t.Log("PARTIAL_BULK=128_WRITTEN REJECTED_ITEM_ABSENT NEXT_BATCH_ABSENT")

	// Make only this isolated source empty by removing its current pointer.
	update := s.db.Model(&model.KnowledgeDocument{}).
		Where("id = ?", f.document.ID).
		Update("current_version_id", nil)
	exportMySQLMust(t, update.Error)
	require(update.RowsAffected == 1, "empty-source fixture update failed")

	emptyRejected := runCLI("empty-rejected", s.name, s.uuid, mappingPath,
		false, 1, "source-check", "not_attempted")
	require(emptyRejected.Result.DocumentCount == 0, "expected empty source")
	es("GET", "/"+emptyRejected.NewIndex, nil, 404)
	require(aliasTarget() == second.NewIndex, "empty rejection changed alias")

	emptyAllowed := runCLI("empty-allowed", s.name, s.uuid, mappingPath,
		true, 0, "complete", "applied")
	require(emptyAllowed.Result.DocumentCount == 0 &&
		emptyAllowed.Result.OldIndex == second.NewIndex,
		"explicit empty publication result mismatch")
	require(aliasTarget() == emptyAllowed.NewIndex, "empty alias target mismatch")
	checkCount(emptyAllowed.NewIndex, 0)

	for _, index := range []string{first.NewIndex, second.NewIndex} {
		checkCount(index, 501)
		checkNoAliases(index)
	}
	checkCount(partial.NewIndex, 128)
	checkNoAliases(partial.NewIndex)

	var protectedAfter any
	exportMySQLMust(t, json.Unmarshal(
		es("GET", "/_alias/rule_chunks_current", nil, 200),
		&protectedAfter,
	))
	require(reflect.DeepEqual(protectedBefore, protectedAfter),
		"established alias changed during isolated acceptance")

	// Keep every ES resource as evidence. Never wildcard-delete shared indexes.
	for i, index := range []string{
		first.NewIndex, second.NewIndex, partial.NewIndex, emptyAllowed.NewIndex,
	} {
		t.Logf("RETAINED_ES_INDEX_%s=%s", strconv.Itoa(i+1), index)
	}
	t.Logf("RETAINED_ES_ALIAS=%s TARGET=%s", alias, emptyAllowed.NewIndex)
	t.Log("PROTECTED_ALIAS_UNCHANGED=PASS LIVE_ACCEPTANCE=PASS")
}
