package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"video-support-agent/internal/evaluation"
)

type liveFinish struct {
	Status     string            `json:"status"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`
	SavedCases int               `json:"saved_cases"`
	Cancelled  bool              `json:"cancelled"`
	Artifacts  map[string]string `json:"artifacts"`
}

func runLive(arguments []string) (returnErr error) {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "evaluation configuration")
	rootPath := flags.String("root", ".", "video-support-agent source root")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 || *configPath == "" {
		return errors.New("run requires config and optional root")
	}

	root, err := filepath.Abs(*rootPath)
	if err != nil {
		return errors.New("cannot resolve project root")
	}
	path := *configPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	input, corpus, err := loadLiveInputs(root, path)
	if err != nil {
		return err
	}

	sourceDigest, err := evaluation.EvidenceHash(input.Sources)
	if err != nil {
		return err
	}
	inputDigest, err := evaluation.EvidenceHash(input)
	if err != nil {
		return err
	}
	binaryDigest, err := scorerBinaryDigest()
	if err != nil {
		return err
	}

	evidenceRoot := filepath.Join(root, ".scratch", "test", "evidence")
	if err := os.MkdirAll(evidenceRoot, 0700); err != nil {
		return errors.New("cannot create evidence parent")
	}
	output := filepath.Join(evidenceRoot, input.Config.RunID)
	if err := os.Mkdir(output, 0700); err != nil {
		return errors.New("run output must be a new directory")
	}

	finish := liveFinish{
		Status: "running", StartedAt: time.Now().UTC(),
		Artifacts: make(map[string]string),
	}
	save := func(name string, value any) error {
		digest, err := evaluation.WriteNewJSON(filepath.Join(output, name), value)
		if err == nil {
			finish.Artifacts[name] = digest
		}
		return err
	}
	defer func() {
		if returnErr != nil {
			finish.Status = "aborted"
		}
		finish.FinishedAt = time.Now().UTC()
		_, err := evaluation.WriteNewJSON(
			filepath.Join(output, "run-finish.json"), finish,
		)
		if err != nil && returnErr == nil {
			returnErr = err
		}
	}()

	if err := save("inputs.json", input); err != nil {
		return err
	}
	if err := save("run-start.json", struct {
		RunID        string `json:"run_id"`
		RunIndex     int    `json:"run_index"`
		Mode         string `json:"mode"`
		PlannedCases int    `json:"planned_cases"`
		InputsSHA256 string `json:"inputs_sha256"`
		SourceSHA256 string `json:"source_sha256"`
		BinarySHA256 string `json:"binary_sha256"`
	}{
		input.Config.RunID, input.Config.RunIndex, input.Config.Mode,
		len(input.Cases), inputDigest, sourceDigest, binaryDigest,
	}); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	startup, cancelStartup := context.WithTimeout(ctx, 60*time.Second)
	clients, err := openLiveClients(startup, input, corpus)
	cancelStartup()
	if err != nil {
		return err
	}
	defer func() {
		if err := clients.Close(); err != nil && returnErr == nil {
			returnErr = errors.New("evaluation resource close failed")
		}
	}()

	if err := save("generation.json", clients.Target); err != nil {
		return err
	}

	modelDigest, err := evaluation.EvidenceHash(struct {
		Config liveConfig `json:"config"`
		Target any        `json:"generation"`
	}{
		Config: input.Config,
		Target: clients.Target,
	})
	if err != nil {
		return err
	}

	plannerModel, answerModel := input.Config.LLMModel, input.Config.LLMModel
	if input.Config.Mode == "retrieval" || input.Config.Mode == "confirm" {
		plannerModel, answerModel = "unused", "unused"
	}
	snapshot := evaluation.ScoreSnapshot{
		SchemaVersion: evaluation.SchemaVersion,
		RunID:         input.Config.RunID,
		Mode:          input.Config.Mode,
		RunIndex:      input.Config.RunIndex,
		PlannedCases:  len(input.Cases),
		Identity: evaluation.EvaluationIdentity{
			SourceSHA256:      sourceDigest,
			InputsSHA256:      inputDigest,
			CorpusSHA256:      corpus.Digest(),
			BindingSHA256:     clients.Binding.Digest(),
			ModelConfigSHA256: modelDigest,
			PlannerModel:      plannerModel,
			AnswerModel:       answerModel,
			EmbeddingModel:    input.Config.Embedding.Model,
		},
		Cases:     make([]evaluation.Case, 0, len(input.Cases)),
		Retrieval: make([]evaluation.RetrievalObservation, 0),
		Agents:    make([]evaluation.AgentObservation, 0),
	}

	for i, c := range input.Cases {
		name := fmt.Sprintf("case-%04d.json", i+1)
		if input.Config.Mode == "retrieval" {
			rows, err := evaluation.RunRetrieval(ctx, clients.Retriever, clients.Binding, input.Config.RunID, []evaluation.Case{c}, 30*time.Second)
			if err != nil || len(rows) != 1 {
				return errors.New("retrieval execution could not produce a case record")
			}
			if err := save(name, rows[0]); err != nil {
				return err
			}
			snapshot.Cases = append(snapshot.Cases, c)
			snapshot.Retrieval = append(snapshot.Retrieval, rows[0].Observation)
		} else {
			rows, err := clients.Runner.Run(
				ctx, input.Config.RunID,
				[]evaluation.Case{c},
				[]evaluation.AgentScenario{input.Scenarios[i]},
			)
			if err != nil || len(rows) != 1 {
				return errors.New("Agent execution could not produce a case record")
			}
			if err := save(name, rows[0]); err != nil {
				return err
			}
			snapshot.Cases = append(snapshot.Cases, rows[0].Case)
			snapshot.Agents = append(snapshot.Agents, rows[0].Attempt.Observation)
		}
		finish.SavedCases++
	}
	finish.Cancelled = ctx.Err() != nil

	afterSources, err := sourceFingerprint(root)
	if err != nil {
		return err
	}
	afterSourceDigest, err := evaluation.EvidenceHash(afterSources)
	if err != nil || afterSourceDigest != sourceDigest {
		return errors.New("source changed during evaluation; partial evidence retained")
	}

	checkCtx, cancelCheck := context.WithTimeout(context.Background(), 45*time.Second)
	checkErr := clients.Check(checkCtx)
	cancelCheck()
	if checkErr != nil {
		return errors.New("runtime identity changed or cannot be verified")
	}

	snapshot, err = evaluation.FreezeScoreSnapshot(snapshot)
	if err != nil {
		return err
	}
	if err := save("score-snapshot.json", snapshot); err != nil {
		return err
	}

	report, err := evaluation.ScoreSnapshotData(snapshot, nil)
	if err != nil {
		return err
	}
	report.SnapshotFileSHA256 = finish.Artifacts["score-snapshot.json"]
	report.ScorerBinarySHA256 = binaryDigest
	if err := save("report-unreviewed.json", report); err != nil {
		return err
	}

	finish.Status = "finished"
	fmt.Println("evaluation records written; inspect report and run-finish.json")
	return nil
}
