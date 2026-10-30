package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"video-support-agent/internal/config"
	"video-support-agent/internal/elasticsearch"
	indexing "video-support-agent/internal/index"
	"video-support-agent/internal/repository"

	driver "github.com/go-sql-driver/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var _ indexing.IndexWriter = (*elasticsearch.Client)(nil)

type options struct {
	expectedDatabase   string
	expectedServerUUID string
	mappingPath        string
	timeout            time.Duration
	allowEmpty         bool
}

type finalReport struct {
	Event        string                  `json:"event"`
	StartedAt    time.Time               `json:"started_at"`
	FinishedAt   time.Time               `json:"finished_at"`
	Stage        string                  `json:"stage"`
	NewIndex     string                  `json:"new_index,omitempty"`
	Result       *indexing.RebuildResult `json:"result,omitempty"`
	ErrorClasses []string                `json:"error_classes"`
	Message      string                  `json:"message"`
	ExitCode     int                     `json:"exit_code"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	startedAt := time.Now().UTC()
	stage := "arguments"
	newIndex := ""

	finish := func(
		code int,
		message string,
		result *indexing.RebuildResult,
		err error,
	) int {
		if result != nil {
			stage = result.Stage
		}

		report := finalReport{
			Event:        "finished",
			StartedAt:    startedAt,
			FinishedAt:   time.Now().UTC(),
			Stage:        stage,
			NewIndex:     newIndex,
			Result:       result,
			ErrorClasses: classifyError(err),
			Message:      message,
			ExitCode:     code,
		}

		if encodeErr := json.NewEncoder(stdout).Encode(report); encodeErr != nil {
			fmt.Fprintln(stderr,
				"Cannot write final report; publication state must be checked.")
			return 1
		}
		return code
	}

	opts, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		// parseOptions returns only our own fixed messages.
		return finish(2, err.Error(), nil, err)
	}

	signalCtx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	ctx, cancel := context.WithTimeout(signalCtx, opts.timeout)
	defer cancel()

	stage = "configuration"
	cfg := config.Load()

	if err := config.ValidateElasticsearchConfig(
		cfg.Elasticsearch,
	); err != nil {
		return finish(
			2, "Invalid Elasticsearch configuration.", nil, err,
		)
	}
	if err := validateDatabaseConfig(cfg.Database, opts); err != nil {
		return finish(2, err.Error(), nil, err)
	}

	stage = "mapping"
	definition, err := readDefinition(opts.mappingPath)
	if err != nil {
		return finish(2, err.Error(), nil, err)
	}
	if err := ctx.Err(); err != nil {
		return finish(1, "Operation canceled or timed out.", nil, err)
	}

	stage = "index-name"
	newIndex, err = generateIndexName(time.Now())
	if err != nil {
		return finish(
			1, "Cannot generate a unique index name.", nil, err,
		)
	}

	// Record the intended resource before any network operation.
	// A hard process kill may prevent the final report.
	startRecord := struct {
		Event     string    `json:"event"`
		StartedAt time.Time `json:"started_at"`
		NewIndex  string    `json:"new_index"`
	}{
		Event:     "started",
		StartedAt: startedAt,
		NewIndex:  newIndex,
	}
	if err := json.NewEncoder(stdout).Encode(startRecord); err != nil {
		fmt.Fprintln(stderr,
			"Cannot write start record; rebuild was not started.")
		return 1
	}

	result, err := execute(ctx, cfg, opts, newIndex, definition)
	if err != nil {
		return finish(
			1,
			"Rebuild did not finish cleanly; inspect stage and publication state.",
			result,
			err,
		)
	}

	return finish(0, "Rebuild completed.", result, nil)
}

func parseOptions(args []string, helpOutput io.Writer) (options, error) {
	var opts options

	flags := flag.NewFlagSet("reindex", flag.ContinueOnError)
	// Flag parsing errors may contain raw user-supplied values.
	flags.SetOutput(io.Discard)

	flags.StringVar(
		&opts.expectedDatabase,
		"expect-db",
		"",
		"Required independently verified source database name",
	)
	flags.StringVar(
		&opts.expectedServerUUID,
		"expect-server-uuid",
		"",
		"Required independently verified MySQL server UUID",
	)
	flags.StringVar(
		&opts.mappingPath,
		"mapping",
		"deploy/elasticsearch/rule-chunks-v1.json",
		"Index definition file",
	)
	flags.DurationVar(
		&opts.timeout,
		"timeout",
		5*time.Minute,
		"Positive timeout for normal rebuild operations",
	)
	flags.BoolVar(
		&opts.allowEmpty,
		"allow-empty",
		false,
		"Explicitly permit publishing an empty snapshot",
	)

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(helpOutput, "Usage: reindex [options]")
			flags.SetOutput(helpOutput)
			flags.PrintDefaults()
			return opts, flag.ErrHelp
		}
		return opts, errors.New("Invalid arguments; use -h for help.")
	}
	if flags.NArg() != 0 {
		return opts, errors.New("Positional arguments are not supported.")
	}

	for _, value := range []string{
		opts.expectedDatabase,
		opts.expectedServerUUID,
	} {
		if !utf8.ValidString(value) ||
			strings.TrimSpace(value) == "" ||
			value != strings.TrimSpace(value) {
			return opts, errors.New(
				"Both expected source identities are required without surrounding whitespace.",
			)
		}
	}

	if opts.timeout <= 0 {
		return opts, errors.New("Timeout must be positive.")
	}
	if strings.TrimSpace(opts.mappingPath) == "" {
		return opts, errors.New("Mapping file path is required.")
	}

	return opts, nil
}

func validateDatabaseConfig(
	cfg config.DatabaseConfig,
	opts options,
) error {
	if strings.TrimSpace(cfg.Host) == "" ||
		strings.TrimSpace(cfg.User) == "" ||
		cfg.Password == "" ||
		strings.TrimSpace(cfg.Name) == "" {
		return errors.New(
			"Database host, user, password and name are required.",
		)
	}

	port, err := strconv.Atoi(cfg.Port)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("Database port must be between 1 and 65535.")
	}
	if cfg.Name != opts.expectedDatabase {
		return errors.New(
			"Configured database does not match -expect-db.",
		)
	}

	return nil
}

func readDefinition(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("Cannot open mapping file.")
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("Mapping must be a regular file.")
	}

	data, err := io.ReadAll(io.LimitReader(
		file,
		int64(elasticsearch.MaxRequestBytes)+1,
	))
	if err != nil {
		return nil, errors.New("Cannot read mapping file.")
	}
	if len(data) == 0 || len(data) > elasticsearch.MaxRequestBytes {
		return nil, errors.New("Mapping file exceeds the allowed size.")
	}

	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, errors.New("Mapping file must contain a JSON object.")
	}
	if _, exists := object["aliases"]; exists {
		return nil, errors.New(
			"Mapping file must not publish aliases during index creation.",
		)
	}

	for _, key := range []string{"settings", "mappings"} {
		var value map[string]json.RawMessage
		if json.Unmarshal(object[key], &value) != nil || value == nil {
			return nil, errors.New(
				"Mapping file requires settings and mappings objects.",
			)
		}
	}

	return data, nil
}

func generateIndexName(now time.Time) (string, error) {
	var randomBytes [12]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", err
	}

	timestamp := strings.ToLower(
		now.UTC().Format("20060102T150405Z"),
	)

	return "rule_chunks_v1_" + timestamp + "_" +
		hex.EncodeToString(randomBytes[:]), nil
}

func execute(
	ctx context.Context,
	cfg config.Config,
	opts options,
	newIndex string,
	definition []byte,
) (result *indexing.RebuildResult, err error) {
	result = &indexing.RebuildResult{
		Stage:    "connect",
		NewIndex: newIndex,
		Publication: indexing.PublicationResult{
			State: indexing.PublicationNotAttempted,
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
			err = errors.Join(err, closeErr)
		}
	}()

	client, err := elasticsearch.NewClient(cfg.Elasticsearch)
	if err != nil {
		return result, err
	}
	defer client.Close()

	source := repository.NewKnowledgeRepository(db)

	return indexing.Rebuild(ctx, source, client, indexing.RebuildOptions{
		NewIndex:           newIndex,
		Definition:         definition,
		ExpectedDatabase:   opts.expectedDatabase,
		ExpectedServerUUID: opts.expectedServerUUID,
		AllowEmpty:         opts.allowEmpty,
	})
}

func openSourceDatabase(
	ctx context.Context,
	cfg config.DatabaseConfig,
) (*gorm.DB, *sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	driverConfig := driver.NewConfig()
	driverConfig.User = cfg.User
	driverConfig.Passwd = cfg.Password
	driverConfig.Net = "tcp"
	driverConfig.Addr = net.JoinHostPort(cfg.Host, cfg.Port)
	driverConfig.DBName = cfg.Name
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	driverConfig.Timeout = 5 * time.Second
	driverConfig.ReadTimeout = 5 * time.Second
	driverConfig.WriteTimeout = 5 * time.Second
	driverConfig.Logger = &driver.NopLogger{}

	connector, err := driver.NewConnector(driverConfig)
	if err != nil {
		return nil, nil, err
	}

	// OpenDB creates a pool; the explicit PingContext below connects.
	pool := sql.OpenDB(connector)
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	pool.SetConnMaxLifetime(30 * time.Minute)
	pool.SetConnMaxIdleTime(5 * time.Minute)

	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, nil, err
	}

	db, err := gorm.Open(gormmysql.New(gormmysql.Config{
		Conn:                      pool,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger: gormlogger.Default.LogMode(
			gormlogger.Silent,
		),
	})
	if err != nil {
		_ = pool.Close()
		return nil, nil, err
	}

	if err := ctx.Err(); err != nil {
		_ = pool.Close()
		return nil, nil, err
	}

	return db, pool, nil
}

func classifyError(err error) []string {
	classes := make([]string, 0)
	if err == nil {
		return classes
	}

	checks := []struct {
		target error
		name   string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
		{indexing.ErrInvalidSource, "invalid_source"},
		{indexing.ErrExportLimitExceeded, "export_limit_exceeded"},
		{elasticsearch.ErrUnavailable, "es_unavailable"},
		{elasticsearch.ErrResponse, "es_response_invalid"},
		{elasticsearch.ErrRejected, "es_rejected"},
		{elasticsearch.ErrStateUnknown, "es_state_unknown"},
	}

	for _, check := range checks {
		if errors.Is(err, check.target) {
			classes = append(classes, check.name)
		}
	}

	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		classes = append(classes, "network_timeout")
	}

	if len(classes) == 0 {
		classes = append(classes, "operation_failed")
	}

	return classes
}
