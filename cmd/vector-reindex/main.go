package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/config"
	"video-support-agent/internal/embedding"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/vectorrebuild"
)

type options struct {
	expectedDatabase   string
	expectedServerUUID string
	timeout            time.Duration
	milvusTimeout      time.Duration
}

type event struct {
	Event         string                `json:"event"`
	Time          time.Time             `json:"time"`
	NewCollection string                `json:"new_collection,omitempty"`
	Stage         string                `json:"stage"`
	Result        *vectorrebuild.Result `json:"result,omitempty"`
	ErrorClasses  []string              `json:"error_classes,omitempty"`
	ExitCode      *int                  `json:"exit_code,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	stage := "arguments"
	name := ""
	encoder := json.NewEncoder(stdout)

	finish := func(code int, result *vectorrebuild.Result, err error) int {
		if result != nil {
			stage = result.Stage
		}
		if encodeErr := encoder.Encode(event{
			Event:         "finished",
			Time:          time.Now().UTC(),
			NewCollection: name,
			Stage:         stage,
			Result:        result,
			ErrorClasses:  classifyError(err),
			ExitCode:      &code,
		}); encodeErr != nil {
			fmt.Fprintln(stderr, "Cannot write final evidence; inspect the recorded target and alias.")
			return 1
		}
		return code
	}

	opts, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "Invalid arguments; use -h for help.")
		return finish(2, nil, err)
	}

	signalCtx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, opts.timeout)
	defer cancel()

	stage = "configuration"
	cfg := config.Load()
	if opts.milvusTimeout > 0 {
		cfg.Milvus.Timeout = opts.milvusTimeout.String()
	}
	if err := config.ValidateEmbeddingConfig(cfg.Embedding); err != nil {
		return finish(2, nil, err)
	}
	if err := config.ValidateMilvusConfig(cfg.Milvus); err != nil {
		return finish(2, nil, err)
	}
	if err := validateDatabaseConfig(cfg.Database, opts.expectedDatabase); err != nil {
		return finish(2, nil, err)
	}

	name, err = milvus.NewCollectionName()
	if err != nil {
		return finish(1, nil, err)
	}

	if err := encoder.Encode(event{
		Event:         "started",
		Time:          time.Now().UTC(),
		NewCollection: name,
		Stage:         "connect",
	}); err != nil {
		fmt.Fprintln(stderr, "Cannot write start evidence; rebuild was not started.")
		return 1
	}

	report := func(result vectorrebuild.Result) error {
		return encoder.Encode(event{
			Event:         "stage",
			Time:          time.Now().UTC(),
			NewCollection: name,
			Stage:         result.Stage,
			Result:        &result,
		})
	}

	stage = "connect"
	result, err := execute(ctx, cfg, opts, name, report)
	if err != nil {
		return finish(1, result, err)
	}
	return finish(0, result, nil)
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var opts options
	flags := flag.NewFlagSet("vector-reindex", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	flags.StringVar(&opts.expectedDatabase, "expect-db", "", "Required independently verified MySQL database name")
	flags.StringVar(&opts.expectedServerUUID, "expect-server-uuid", "", "Required independently verified MySQL server UUID")
	flags.DurationVar(&opts.timeout, "timeout", 15*time.Minute, "Overall rebuild timeout; publication observation may add up to 10s")
	flags.DurationVar(&opts.milvusTimeout, "milvus-timeout", 0, "Optional per-request Milvus timeout; 0 uses MILVUS_TIMEOUT")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(output, "Usage: vector-reindex [options]")
			flags.SetOutput(output)
			flags.PrintDefaults()
			return opts, flag.ErrHelp
		}
		return opts, errors.New("invalid command arguments")
	}
	if flags.NArg() != 0 || opts.timeout <= 0 || opts.milvusTimeout < 0 {
		return opts, errors.New("invalid command options")
	}
	for _, value := range []string{opts.expectedDatabase, opts.expectedServerUUID} {
		if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
			return opts, errors.New("expected source identities are required")
		}
	}
	return opts, nil
}

func execute(ctx context.Context, cfg config.Config, opts options, name string, report func(vectorrebuild.Result) error) (result *vectorrebuild.Result, err error) {
	result = &vectorrebuild.Result{
		Stage:         "connect",
		NewCollection: name,
		Publication: milvus.PublicationResult{
			State: milvus.PublicationNotAttempted,
		},
	}

	db, pool, err := openSourceDatabase(ctx, cfg.Database)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := pool.Close(); closeErr != nil {
			if err == nil {
				result.Stage = "close"
			}
			err = errors.Join(err, errors.New("database close failed"))
		}
	}()

	model, err := embedding.NewClient(cfg.Embedding)
	if err != nil {
		return result, err
	}
	defer model.Close()

	writer, err := milvus.NewClient(cfg.Milvus)
	if err != nil {
		return result, err
	}
	defer writer.Close()

	return vectorrebuild.Rebuild(
		ctx,
		repository.NewKnowledgeRepository(db),
		model,
		writer,
		vectorrebuild.Options{
			NewCollection:      name,
			ExpectedDatabase:   opts.expectedDatabase,
			ExpectedServerUUID: opts.expectedServerUUID,
			ExpectedDigest:     cfg.Embedding.ModelDigest,
		},
		report,
	)
}

func classifyError(err error) []string {
	if err == nil {
		return nil
	}
	checks := []struct {
		err  error
		name string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{indexing.ErrInvalidSource, "invalid_source"},
		{indexing.ErrExportLimitExceeded, "export_limit_exceeded"},
		{embedding.ErrInput, "embedding_input"},
		{embedding.ErrInputTooLarge, "embedding_input_too_large"},
		{embedding.ErrContextLength, "embedding_context_length"},
		{embedding.ErrUnavailable, "embedding_unavailable"},
		{embedding.ErrRejected, "embedding_rejected"},
		{embedding.ErrResponse, "embedding_response_invalid"},
		{embedding.ErrModelMismatch, "model_mismatch"},
		{milvus.ErrInput, "milvus_input"},
		{milvus.ErrUnavailable, "milvus_unavailable"},
		{milvus.ErrResponse, "milvus_response_invalid"},
		{milvus.ErrRejected, "milvus_rejected"},
		{milvus.ErrStateUnknown, "milvus_state_unknown"},
		{vectorrebuild.ErrEvidence, "evidence_write_failed"},
	}
	classes := make([]string, 0)
	for _, check := range checks {
		if errors.Is(err, check.err) {
			classes = append(classes, check.name)
		}
	}
	if len(classes) == 0 {
		classes = append(classes, "operation_failed")
	}
	return classes
}
