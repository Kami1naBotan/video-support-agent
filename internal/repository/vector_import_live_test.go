//go:build indexexportintegration && vectorimportintegration

package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/model"
	"video-support-agent/internal/vectorization"
	"video-support-agent/internal/vectorrebuild"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const vectorLiveDigest = "7907646426070047a77226ac3e684fbbe8410524f7b4a74d02837e43f2146bab"

var vectorLiveFields = []string{
	"chunk_id", "document_id", "version_id", "article_id",
	"category", "publication_revision", "version", "embedding",
	"effective_at", "has_expires_at", "expires_at",
}

type vectorLiveEvent struct {
	Event         string                `json:"event"`
	Stage         string                `json:"stage"`
	NewCollection string                `json:"new_collection"`
	Result        *vectorrebuild.Result `json:"result"`
	ErrorClasses  []string              `json:"error_classes"`
	ExitCode      *int                  `json:"exit_code"`
}

type vectorLiveHTTP struct {
	t       *testing.T
	dir     string
	client  *http.Client
	seq     int
	mutable map[string]bool
}

func vectorLiveRequire(t *testing.T, ok bool, message string) {
	t.Helper()
	if !ok {
		t.Fatal(message)
	}
}

func vectorLiveMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		message := err.Error()
		if password := os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD"); password != "" {
			message = strings.ReplaceAll(message, password, "<redacted>")
		}
		t.Fatal(message)
	}
}

func vectorLiveJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	vectorLiveMust(t, err)
	return data
}

func vectorLiveSave(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	vectorLiveMust(t, os.WriteFile(filepath.Join(dir, name), data, 0600))
}

func (h *vectorLiveHTTP) call(path string, payload map[string]any) json.RawMessage {
	h.t.Helper()
	allowed := map[string]bool{
		"aliases/describe": true, "collections/describe": true,
		"indexes/describe": true, "entities/query": true,
		"collections/list": true, "aliases/list": true,
	}
	if !allowed[path] {
		name, _ := payload["collectionName"].(string)
		vectorLiveRequire(h.t,
			(path == "entities/upsert" || path == "collections/flush") &&
				h.mutable[name],
			"raw write attempted outside the unpublished test target")
	}
	requestBody := map[string]any{"dbName": "default"}
	for key, value := range payload {
		requestBody[key] = value
	}
	h.seq++
	stem := fmt.Sprintf("%03d-%s", h.seq, strings.ReplaceAll(path, "/", "-"))
	raw := vectorLiveJSON(h.t, requestBody)
	vectorLiveSave(h.t, h.dir, stem+"-request.json", raw)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		"http://127.0.0.1:19530/v2/vectordb/"+path,
		bytes.NewReader(raw),
	)
	vectorLiveMust(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := h.client.Do(req)
	vectorLiveMust(h.t, err)
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	vectorLiveSave(h.t, h.dir, stem+"-response.json", body)
	vectorLiveSave(h.t, h.dir, stem+"-status.txt",
		[]byte(strconv.Itoa(response.StatusCode)))
	vectorLiveMust(h.t, err)
	vectorLiveRequire(h.t, len(body) <= 4<<20, "response exceeded evidence limit")
	vectorLiveRequire(h.t, response.StatusCode == 200, "Milvus HTTP request failed")

	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	vectorLiveMust(h.t, json.Unmarshal(body, &envelope))
	vectorLiveRequire(h.t, envelope.Code != nil && *envelope.Code == 0,
		"Milvus business error; inspect recorded response")
	vectorLiveRequire(h.t, len(envelope.Data) != 0 &&
		string(bytes.TrimSpace(envelope.Data)) != "null", "missing response data")
	return envelope.Data
}

func (h *vectorLiveHTTP) count(name string) int64 {
	h.t.Helper()
	data := h.call("entities/query", map[string]any{
		"collectionName": name, "filter": "",
		"outputFields": []string{"count(*)"}, "consistencyLevel": "Strong",
	})
	var rows []struct {
		Count *int64 `json:"count(*)"`
	}
	vectorLiveMust(h.t, json.Unmarshal(data, &rows))
	vectorLiveRequire(h.t, len(rows) == 1 && rows[0].Count != nil,
		"invalid count response")
	return *rows[0].Count
}

func (h *vectorLiveHTTP) protectedSnapshot() map[string]any {
	h.t.Helper()
	result := make(map[string]any)
	for _, item := range []struct {
		alias, collection string
		id, count         int64
	}{
		{
			"rule_chunks_vector_current",
			"rule_chunks_vec_v1_20261031_104029_fbea58f5",
			470140144896710144, 0,
		},
		{
			"milvus_it_alias_20261031_105720_a256859f",
			"milvus_it_20261031_105720_a256859f",
			470140144896715546, 1,
		},
	} {
		aliasRaw := h.call("aliases/describe", map[string]any{"aliasName": item.alias})
		var alias struct {
			AliasName      string `json:"aliasName"`
			CollectionName string `json:"collectionName"`
			DBName         string `json:"dbName"`
		}
		vectorLiveMust(h.t, json.Unmarshal(aliasRaw, &alias))
		vectorLiveRequire(h.t,
			alias.AliasName == item.alias &&
				alias.CollectionName == item.collection && alias.DBName == "default",
			"protected alias differs from established baseline")

		raw := h.call("collections/describe",
			map[string]any{"collectionName": item.collection})
		var identity struct {
			ID int64 `json:"collectionID"`
		}
		vectorLiveMust(h.t, json.Unmarshal(raw, &identity))
		vectorLiveRequire(h.t, identity.ID == item.id, "protected collection ID changed")

		var description map[string]json.RawMessage
		vectorLiveMust(h.t, json.Unmarshal(raw, &description))
		stable := make(map[string]json.RawMessage)
		for _, key := range []string{
			"collectionName", "collectionID", "description", "autoId",
			"enableDynamicField", "consistencyLevel", "shardsNum", "fields",
		} {
			value, ok := description[key]
			vectorLiveRequire(h.t, ok, "protected description missing "+key)
			stable[key] = value
		}
		vectorLiveRequire(h.t, h.count(item.collection) == item.count,
			"protected collection count changed")
		result[item.alias] = map[string]any{
			"alias": alias, "schema": stable, "count": item.count,
		}
	}
	sentinel := h.call("entities/query", map[string]any{
		"collectionName": "milvus_it_20261031_105720_a256859f",
		"filter":         `chunk_id == "it_sentinel"`,
		"outputFields":   vectorLiveFields,
		"limit":          1, "consistencyLevel": "Strong",
	})
	var rows []json.RawMessage
	vectorLiveMust(h.t, json.Unmarshal(sentinel, &rows))
	vectorLiveRequire(h.t, len(rows) == 1, "S01 sentinel missing")
	result["sentinel"] = rows[0]
	return result
}

func vectorLiveEnvironment(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		replaced := false
		for name := range overrides {
			if strings.EqualFold(key, name) {
				replaced = true
				break
			}
		}
		if !replaced {
			env = append(env, entry)
		}
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func TestVectorImportLive(t *testing.T) {
	vectorLiveRequire(t, os.Getenv("RUN_VECTOR_IMPORT_LIVE") == "1",
		"RUN_VECTOR_IMPORT_LIVE=1 is required; this test does not skip")
	vectorLiveRequire(t, os.Getenv("INDEX_EXPORT_MYSQL_ADDR") == "127.0.0.1:13308",
		"isolated MySQL address must be 127.0.0.1:13308")
	vectorLiveRequire(t, os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD") != "",
		"isolated MySQL password is required")
	expectedUUID := os.Getenv("INDEX_EXPORT_EXPECT_SERVER_UUID")
	vectorLiveRequire(t, expectedUUID == "9ae2c7f4-d375-11f1-8945-ce027cf8cce1",
		"isolated MySQL UUID differs from established baseline")

	root := os.Getenv("VECTOR_IMPORT_PROJECT_ROOT")
	dir := os.Getenv("VECTOR_IMPORT_EVIDENCE")
	vectorLiveRequire(t, filepath.IsAbs(root) && filepath.IsAbs(dir),
		"absolute project/evidence paths required")
	executable := filepath.Join(dir, "vector-reindex.exe")
	info, err := os.Stat(executable)
	vectorLiveMust(t, err)
	vectorLiveRequire(t, info.Mode().IsRegular(), "CLI executable missing")

	httpClient := &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(httpClient.CloseIdleConnections)
	h := &vectorLiveHTTP{
		t: t, dir: dir, client: httpClient, mutable: make(map[string]bool),
	}
	protectedBefore := h.protectedSnapshot()
	vectorLiveSave(t, dir, "protected-before.json", vectorLiveJSON(t, protectedBefore))
	t.Cleanup(func() {
		after := h.protectedSnapshot()
		vectorLiveSave(t, dir, "protected-after.json", vectorLiveJSON(t, after))
		vectorLiveRequire(t, reflect.DeepEqual(protectedBefore, after),
			"protected resources changed during integration test")
	})

	// 复用连接助手，但不调用旧套件的建库/自动删库逻辑。
	_, admin := exportMySQLOpen(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var actualUUID, mysqlVersion string
	vectorLiveMust(t, admin.QueryRowContext(ctx,
		"SELECT @@server_uuid, VERSION()").Scan(&actualUUID, &mysqlVersion))
	vectorLiveRequire(t, actualUUID == expectedUUID &&
		strings.HasPrefix(mysqlVersion, "8.4."), "unexpected MySQL instance/version")

	var random [8]byte
	_, err = rand.Read(random[:])
	vectorLiveMust(t, err)
	suffix := hex.EncodeToString(random[:])
	databaseName := "vec_import_it_" + suffix
	aliasName := "vec_import_it_" + suffix

	vectorLiveSave(t, dir, "resources.json", vectorLiveJSON(t, map[string]any{
		"database": databaseName, "server_uuid": actualUUID,
		"mysql_version": mysqlVersion, "alias": aliasName,
		"mysql_address": "127.0.0.1:13308",
		"milvus_url":    "http://127.0.0.1:19530",
		"ollama_url":    "http://127.0.0.1:11434",
		"fixture_kind":  "synthetic_rules_not_official_sources",
		"cleanup":       "resources_retained",
	}))
	_, err = admin.ExecContext(ctx,
		"CREATE DATABASE `"+databaseName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci")
	vectorLiveMust(t, err)
	t.Logf("PRESERVED_TEST_DATABASE=%s TEST_ALIAS=%s", databaseName, aliasName)

	db, pool := exportMySQLOpen(t, databaseName)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	vectorLiveMust(t, db.WithContext(ctx).
		Set("gorm:table_options", "ENGINE=InnoDB").AutoMigrate(
		&model.KnowledgeDocument{}, &model.RuleCategory{},
		&model.RuleVersion{}, &model.RuleArticle{}, &model.KnowledgeChunk{},
	))
	var engines int
	vectorLiveMust(t, pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = ? AND engine = 'InnoDB'
		  AND table_name IN (
		    'knowledge_documents','rule_categories','rule_versions',
		    'rule_articles','knowledge_chunks'
		  )`, databaseName).Scan(&engines))
	vectorLiveRequire(t, engines == 5, "expected five real InnoDB tables")

	at := time.Now().UTC().Truncate(time.Millisecond)
	effective, expires := at.Add(-time.Hour), at.Add(365*24*time.Hour)
	texts := make([]string, 17)
	for i := range texts {
		texts[i] = fmt.Sprintf(
			"【虚构测试规则，不作为官方依据】第%d条：用户提交测试材料时，应填写材料编号并保存提交记录。",
			i+1,
		)
	}
	vectorLiveMust(t, db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		document := model.KnowledgeDocument{DocumentKey: "vector_import_fixture"}
		category := model.RuleCategory{Code: "vector_test", Name: "虚构测试分类"}
		if err := tx.Create(&document).Error; err != nil {
			return err
		}
		if err := tx.Create(&category).Error; err != nil {
			return err
		}
		version := model.RuleVersion{
			DocumentID: document.ID, CategoryID: category.ID,
			Version: "test-v1", Title: "隔离向量导入测试",
			Content:     strings.Join(texts, "\n"),
			SourceURL:   "https://example.invalid/vector-import-fixture",
			Status:      model.RuleVersionStatusDraft,
			EffectiveAt: &effective, ExpiresAt: &expires,
		}
		if err := tx.Omit(clause.Associations).Create(&version).Error; err != nil {
			return err
		}
		for i, text := range texts {
			article := model.RuleArticle{
				VersionID: version.ID, ArticleOrder: uint32(i + 1),
				SectionPath: "虚构测试章节", ArticleNumber: strconv.Itoa(i + 1),
				Content: text,
			}
			if err := tx.Omit(clause.Associations).Create(&article).Error; err != nil {
				return err
			}
			chunk := model.KnowledgeChunk{
				ArticleID: article.ID, ChunkIndex: 0, Content: text,
			}
			if err := tx.Omit(clause.Associations).Create(&chunk).Error; err != nil {
				return err
			}
		}
		// 测试夹具在内容完整写入后才设置发布状态和当前指针。
		if err := tx.Model(&model.RuleVersion{}).Where("id = ?", version.ID).
			Updates(map[string]any{
				"status": model.RuleVersionStatusPublished, "published_at": at,
			}).Error; err != nil {
			return err
		}
		return tx.Model(&model.KnowledgeDocument{}).Where("id = ?", document.ID).
			Updates(map[string]any{
				"current_version_id": version.ID, "publication_revision": 1,
			}).Error
	}))

	repository := NewKnowledgeRepository(db)
	source, err := repository.ExportCurrent(ctx)
	vectorLiveMust(t, err)
	vectorLiveRequire(t, source.SourceDatabase == databaseName &&
		source.SourceServerUUID == actualUUID && len(source.Documents) == 17,
		"real export identity/count mismatch")
	for i, document := range source.Documents {
		vectorLiveRequire(t, document.Content == texts[i], "export order/text mismatch")
	}
	vectorLiveRequire(t, pool.Stats().InUse == 0,
		"export returned while retaining a connection")
	vectorLiveSave(t, dir, "source-before.json", vectorLiveJSON(t, source))

	embedder, err := embedding.NewClient(config.EmbeddingConfig{
		URL: "http://127.0.0.1:11434", Model: "bge-m3:567m",
		ModelDigest: vectorLiveDigest, Dimensions: "1024",
		ContextLength: "8192", Timeout: "120s",
	})
	vectorLiveMust(t, err)
	t.Cleanup(embedder.Close)
	vectorCtx, vectorCancel := context.WithTimeout(ctx, 5*time.Minute)
	vectors, err := vectorization.Vectorize(vectorCtx, source, embedder)
	vectorCancel()
	vectorLiveMust(t, err)
	vectorLiveRequire(t, vectors.Identity.ServerVersion == "0.40.13",
		"Ollama version changed; reassess baseline before proceeding")
	prepared, err := milvus.PrepareImport(ctx, vectors, vectorLiveDigest)
	vectorLiveMust(t, err)
	vectorLiveSave(t, dir, "reference-import.json", vectorLiveJSON(t, prepared))

	writer, err := milvus.NewClient(config.MilvusConfig{
		URL: "http://127.0.0.1:19530", Database: "default",
		CollectionAlias: aliasName, Timeout: "60s",
	})
	vectorLiveMust(t, err)
	t.Cleanup(writer.Close)
	old, err := writer.GetAliasTarget(ctx)
	vectorLiveMust(t, err)
	vectorLiveRequire(t, old == "", "generated test alias already exists")

	runCLI := func(label, uuid, digest string, wantExit int, wantStage, wantClass string) vectorLiveEvent {
		t.Helper()
		commandCtx, commandCancel := context.WithTimeout(ctx, 6*time.Minute)
		defer commandCancel()
		command := exec.CommandContext(commandCtx, executable,
			"-expect-db", databaseName, "-expect-server-uuid", uuid,
			"-timeout", "5m", "-milvus-timeout", "60s",
		)
		command.Dir = root
		command.Env = vectorLiveEnvironment(map[string]string{
			"DB_HOST": "127.0.0.1", "DB_PORT": "13308", "DB_USER": "root",
			"DB_PASSWORD":            os.Getenv("ES_TEST_MYSQL_ROOT_PASSWORD"),
			"DB_NAME":                databaseName,
			"EMBEDDING_URL":          "http://127.0.0.1:11434",
			"EMBEDDING_MODEL":        "bge-m3:567m",
			"EMBEDDING_MODEL_DIGEST": digest,
			"EMBEDDING_DIMENSIONS":   "1024", "EMBEDDING_CONTEXT_LENGTH": "8192",
			"EMBEDDING_TIMEOUT": "120s",
			"MILVUS_URL":        "http://127.0.0.1:19530",
			"MILVUS_DATABASE":   "default", "MILVUS_COLLECTION_ALIAS": aliasName,
			"MILVUS_TIMEOUT": "60s",
		})
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		runErr := command.Run()
		vectorLiveSave(t, dir, label+".jsonl", stdout.Bytes())
		vectorLiveSave(t, dir, label+".stderr.log", stderr.Bytes())
		code := 0
		if runErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) {
				vectorLiveMust(t, runErr)
			}
			code = exitErr.ExitCode()
		}
		vectorLiveSave(t, dir, label+".exit.log",
			[]byte(fmt.Sprintf("EXIT_CODE=%d\n", code)))
		vectorLiveRequire(t, commandCtx.Err() == nil && code == wantExit,
			label+": unexpected process outcome; inspect saved logs")

		var events []vectorLiveEvent
		decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
		for {
			var event vectorLiveEvent
			err := decoder.Decode(&event)
			if err == io.EOF {
				break
			}
			vectorLiveMust(t, err)
			events = append(events, event)
		}
		vectorLiveRequire(t, len(events) >= 3, "missing CLI stage evidence")
		first, final := events[0], events[len(events)-1]
		vectorLiveRequire(t, first.Event == "started" && final.Event == "finished" &&
			first.NewCollection != "" && first.NewCollection == final.NewCollection &&
			final.Stage == wantStage && final.ExitCode != nil &&
			*final.ExitCode == wantExit && final.Result != nil,
			"CLI final/start evidence mismatch")
		vectorLiveRequire(t, final.Result.NewCollection == final.NewCollection,
			"result resource identity differs")

		expectedStages := []string{
			"alias-read", "export", "source-check", "vectorize",
			"prepare-import", "build", "verify", "publish", "complete",
		}
		var actualStages []string
		for _, event := range events[1 : len(events)-1] {
			vectorLiveRequire(t, event.Event == "stage" &&
				event.NewCollection == final.NewCollection &&
				event.Result != nil && event.Result.Stage == event.Stage,
				"invalid intermediate CLI event")
			actualStages = append(actualStages, event.Stage)
		}
		end := -1
		for i, stage := range expectedStages {
			if stage == wantStage {
				end = i
				break
			}
		}
		vectorLiveRequire(t, end >= 0 &&
			reflect.DeepEqual(actualStages, expectedStages[:end+1]),
			"CLI stage order differs")
		if wantExit == 0 {
			vectorLiveRequire(t, len(final.ErrorClasses) == 0 &&
				final.Result.Count == 17 &&
				final.Result.Identity == prepared.Identity &&
				final.Result.Publication.State == milvus.PublicationApplied &&
				final.Result.Verification != nil,
				"successful CLI report missing validated import/publication")
		} else {
			found := false
			for _, class := range final.ErrorClasses {
				found = found || class == wantClass
			}
			vectorLiveRequire(t, found &&
				final.Result.Publication.State == milvus.PublicationNotAttempted,
				"failure classification/publication mismatch")
		}
		return final
	}

	checkTarget := func(name string, expectedID int64) {
		t.Helper()
		id, err := writer.VerifyCollectionContract(ctx, name, prepared.Identity)
		vectorLiveMust(t, err)
		vectorLiveRequire(t, id == expectedID && h.count(name) == 17,
			"stored target identity/count differs")
		raw := h.call("entities/query", map[string]any{
			"collectionName": name, "filter": "",
			"outputFields": vectorLiveFields, "limit": 17,
			"consistencyLevel": "Strong",
		})
		var rows []json.RawMessage
		vectorLiveMust(t, json.Unmarshal(raw, &rows))
		vectorLiveRequire(t, len(rows) == 17, "full record readback missing rows")
		expected := make(map[string]milvus.Record)
		for _, record := range prepared.Records {
			expected[record.ChunkID] = record
		}
		for _, row := range rows {
			var fields map[string]json.RawMessage
			vectorLiveMust(t, json.Unmarshal(row, &fields))
			vectorLiveRequire(t, len(fields) == 11, "stored record field count differs")
			for _, field := range vectorLiveFields {
				value, ok := fields[field]
				vectorLiveRequire(t, ok &&
					string(bytes.TrimSpace(value)) != "null", "missing/null stored field")
			}
			var actual milvus.Record
			vectorLiveMust(t, json.Unmarshal(row, &actual))
			want, ok := expected[actual.ChunkID]
			vectorLiveRequire(t, ok, "unexpected or duplicate stored ID")
			delete(expected, actual.ChunkID)

			av, wv := actual.Embedding, want.Embedding
			actual.Embedding, want.Embedding = nil, nil
			vectorLiveRequire(t, reflect.DeepEqual(actual, want),
				"stored metadata differs from real exported source")
			vectorLiveRequire(t, len(av) == 1024 && len(wv) == 1024,
				"stored/reference vector dimensions differ")
			var dot, aa, bb float64
			for i, v := range av {
				x, y := float64(v), float64(wv[i])
				vectorLiveRequire(t, !math.IsNaN(x) && !math.IsInf(x, 0),
					"stored vector contains non-finite value")
				dot += x * y
				aa += x * x
				bb += y * y
			}
			vectorLiveRequire(t, aa > 0 && bb > 0 &&
				dot/math.Sqrt(aa*bb) >= 0.999,
				"stored vector differs from independently generated reference")
		}
		vectorLiveRequire(t, len(expected) == 0, "missing stored IDs")
	}

	first := runCLI("first", actualUUID, vectorLiveDigest, 0, "complete", "")
	vectorLiveRequire(t, first.Result.OldCollection == "", "first import found unexpected alias")
	checkTarget(first.NewCollection, first.Result.Verification.CollectionID)
	current, err := writer.GetAliasTarget(ctx)
	vectorLiveMust(t, err)
	vectorLiveRequire(t, current == first.NewCollection, "first publication not visible")

	second := runCLI("second", actualUUID, vectorLiveDigest, 0, "complete", "")
	vectorLiveRequire(t, second.NewCollection != first.NewCollection &&
		second.Result.OldCollection == first.NewCollection,
		"repeat rebuild did not use a new target")
	checkTarget(second.NewCollection, second.Result.Verification.CollectionID)
	checkTarget(first.NewCollection, first.Result.Verification.CollectionID)

	for _, failure := range []struct {
		label, uuid, digest, stage, class string
	}{
		{"wrong-source", "wrong-server", vectorLiveDigest, "source-check", "invalid_source"},
		{"wrong-model", actualUUID, strings.Repeat("b", 64), "vectorize", "model_mismatch"},
	} {
		result := runCLI(failure.label, failure.uuid, failure.digest, 1, failure.stage, failure.class)
		target, err := writer.GetAliasTarget(ctx)
		vectorLiveMust(t, err)
		vectorLiveRequire(t, target == second.NewCollection, "failed rebuild changed alias")
		var collections []string
		vectorLiveMust(t, json.Unmarshal(h.call("collections/list", nil), &collections))
		for _, name := range collections {
			vectorLiveRequire(t, name != result.NewCollection,
				"pre-build failure unexpectedly created a target")
		}
	}

	// 创建未发布的独立目标，真实写入后注入元数据不一致。
	damagedName, err := milvus.NewCollectionName()
	vectorLiveMust(t, err)
	h.mutable[damagedName] = true
	vectorLiveSave(t, dir, "damaged-target.json",
		vectorLiveJSON(t, map[string]string{"collection": damagedName}))
	buildCtx, buildCancel := context.WithTimeout(ctx, 5*time.Minute)
	build, err := writer.BuildCollection(buildCtx, damagedName, prepared)
	buildCancel()
	vectorLiveMust(t, err)
	vectorLiveSave(t, dir, "damaged-build.json", vectorLiveJSON(t, build))
	badRecord := prepared.Records[0]
	badRecord.Version = "corrupted-test-version"
	h.call("entities/upsert", map[string]any{
		"collectionName": damagedName, "data": []milvus.Record{badRecord},
	})
	// BuildCollection 已对该集合执行过 flush。
	// 本环境返回的 flush 限额为 0.1 次/秒；第二次请求留出 11 秒间隔。
	// 这里只控制测试请求频率，不重试写入，也不据等待时间判定数据就绪。
	t.Log("Waiting 11s before the second flush on the unpublished test target")
	flushInterval := time.NewTimer(11 * time.Second)
	defer flushInterval.Stop()

	select {
	case <-ctx.Done():
		t.Fatalf("waiting for flush interval: %v", ctx.Err())
	case <-flushInterval.C:
	}
	vectorLiveMust(t, ctx.Err())

	h.call("collections/flush", map[string]any{"collectionName": damagedName})
	token, verifyErr := writer.VerifyCollection(ctx, build, prepared)
	vectorLiveRequire(t, token == nil && errors.Is(verifyErr, milvus.ErrResponse),
		"real verification accepted corrupted metadata")
	vectorLiveSave(t, dir, "damaged-verification.json", vectorLiveJSON(t, map[string]any{
		"rejected": true, "error": verifyErr.Error(), "token_returned": false,
	}))
	current, err = writer.GetAliasTarget(ctx)
	vectorLiveMust(t, err)
	vectorLiveRequire(t, current == second.NewCollection && h.count(damagedName) == 17,
		"verification failure changed alias or removed failed target")
	checkTarget(second.NewCollection, second.Result.Verification.CollectionID)

	afterSource, err := repository.ExportCurrent(ctx)
	vectorLiveMust(t, err)
	vectorLiveRequire(t, reflect.DeepEqual(source.Documents, afterSource.Documents),
		"import modified MySQL source content")
	vectorLiveSave(t, dir, "source-after.json", vectorLiveJSON(t, afterSource))
	vectorLiveRequire(t, pool.Stats().InUse == 0, "source connection remains in use")

	var otherSessions int
	vectorLiveMust(t, pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.processlist
		WHERE DB = ? AND ID <> CONNECTION_ID()
	`, databaseName).Scan(&otherSessions))
	vectorLiveRequire(t, otherSessions == 0,
		"CLI sessions remain connected to the isolated database")

	vectorLiveSave(t, dir, "live-summary.json", vectorLiveJSON(t, map[string]any{
		"status":  "VECTOR_IMPORT_LIVE_PASSED",
		"records": 17, "database": databaseName, "alias": aliasName,
		"first_collection":           first.NewCollection,
		"second_collection":          second.NewCollection,
		"retained_failed_collection": damagedName,
		"real_mysql_export":          true, "real_ollama_vectorization": true,
		"real_cli_rebuild_twice": true, "full_readback_verified": true,
		"source_and_model_failure_preserved_alias": true,
		"corrupted_target_rejected":                true,
		"source_unchanged":                         true, "remaining_cli_sessions": otherSessions,
		"retrieval_quality_verified": false,
		"resources_retained":         true,
	}))
	t.Log("VECTOR_IMPORT_LIVE_PASSED")
}
