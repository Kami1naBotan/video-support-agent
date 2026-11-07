package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"video-support-agent/internal/config"
	"video-support-agent/internal/evaluation"
	"video-support-agent/internal/service"
)

type evaluationDatabase struct {
	Host       string `json:"host"`
	Port       string `json:"port"`
	User       string `json:"user"`
	Name       string `json:"name"`
	ServerUUID string `json:"server_uuid"`
}

type liveConfig struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	RunIndex      int    `json:"run_index"`
	Mode          string `json:"mode"`

	CasesFile     string `json:"cases_file"`
	CorpusFile    string `json:"corpus_file"`
	BindingsFile  string `json:"bindings_file"`
	ScenariosFile string `json:"scenarios_file"`
	GenerationID  string `json:"generation_id"`

	Database      evaluationDatabase         `json:"database"`
	Elasticsearch config.ElasticsearchConfig `json:"elasticsearch"`
	Embedding     config.EmbeddingConfig     `json:"embedding"`
	Milvus        config.MilvusConfig        `json:"milvus"`

	LLMEndpoint string `json:"llm_endpoint"`
	LLMModel    string `json:"llm_model"`
	LLMTimeout  string `json:"llm_timeout"`
}

type liveInputs struct {
	Config    liveConfig                  `json:"config"`
	Cases     []evaluation.Case           `json:"cases"`
	Corpus    evaluation.CorpusManifest   `json:"corpus"`
	Bindings  []evaluation.ArticleBinding `json:"bindings"`
	Scenarios []evaluation.AgentScenario  `json:"scenarios"`
	FileSHA   map[string]string           `json:"file_sha256"`
	Sources   []sourceFile                `json:"sources"`
}

func loadLiveInputs(root, configPath string) (liveInputs, *evaluation.Corpus, error) {
	var input liveInputs
	input.FileSHA = make(map[string]string)

	digest, err := evaluation.ReadStrictJSON(configPath, &input.Config)
	if err != nil {
		return input, nil, err
	}
	input.FileSHA["config"] = digest
	if err := validateLiveConfig(input.Config); err != nil {
		return input, nil, err
	}

	resolve := func(relative string) (string, error) {
		if relative == "" || filepath.IsAbs(relative) {
			return "", errors.New("input paths must be project-relative")
		}
		clean := filepath.Clean(relative)
		if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return "", errors.New("input path escapes project")
		}
		return filepath.Join(root, clean), nil
	}

	casesPath, err := resolve(input.Config.CasesFile)
	if err != nil {
		return input, nil, err
	}
	raw, err := readLimitedFile(casesPath, evaluation.MaxDatasetBytes)
	if err != nil {
		return input, nil, err
	}
	input.FileSHA["cases"] = fileBytesDigest(raw)
	input.Cases, err = evaluation.LoadCases(bytes.NewReader(raw))
	if err != nil {
		return input, nil, err
	}

	for _, entry := range []struct {
		name string
		path string
		dst  any
	}{
		{"corpus", input.Config.CorpusFile, &input.Corpus},
		{"bindings", input.Config.BindingsFile, &input.Bindings},
		{"scenarios", input.Config.ScenariosFile, &input.Scenarios},
	} {
		path, err := resolve(entry.path)
		if err != nil {
			return input, nil, err
		}
		digest, err := evaluation.ReadStrictJSON(path, entry.dst)
		if err != nil {
			return input, nil, err
		}
		input.FileSHA[entry.name] = digest
	}

	corpus, err := evaluation.NewCorpus(input.Corpus)
	if err != nil {
		return input, nil, err
	}
	if !corpus.Synthetic() {
		return input, nil, errors.New("live evaluation requires synthetic corpus")
	}
	if err := corpus.ValidateCases(input.Cases); err != nil {
		return input, nil, err
	}
	if err := validateLiveCases(input); err != nil {
		return input, nil, err
	}

	input.Sources, err = sourceFingerprint(root)
	return input, corpus, err
}

func validateLiveConfig(c liveConfig) error {
	if c.SchemaVersion != evaluation.SchemaVersion || c.RunIndex < 1 ||
		!regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`).MatchString(c.RunID) ||
		!regexp.MustCompile(`^eval_[a-z0-9_]{1,48}$`).MatchString(c.Database.Name) {
		return errors.New("invalid evaluation run or database identity")
	}
	switch c.Mode {
	case "retrieval", "chat", "confirm", "ticket_query":
	default:
		return errors.New("unsupported live mode")
	}
	if service.ValidateAppealOperationKey(c.GenerationID) != nil ||
		!regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(c.Database.ServerUUID) {
		return errors.New("invalid generation or server identity")
	}

	ip := net.ParseIP(c.Database.Host)
	if ip == nil || !ip.IsLoopback() || c.Database.User == "" {
		return errors.New("evaluation database must use an explicit loopback address")
	}
	if err := config.ValidateElasticsearchConfig(c.Elasticsearch); err != nil {
		return errors.New("invalid Elasticsearch configuration")
	}
	if err := config.ValidateEmbeddingConfig(c.Embedding); err != nil {
		return errors.New("invalid embedding configuration")
	}
	if err := config.ValidateMilvusConfig(c.Milvus); err != nil {
		return errors.New("invalid Milvus configuration")
	}
	for _, address := range []string{c.Elasticsearch.URL, c.Milvus.URL, c.Embedding.URL} {
		endpoint, err := url.Parse(address)
		if err != nil {
			return errors.New("invalid local service endpoint")
		}
		ip := net.ParseIP(endpoint.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return errors.New("evaluation data services must use loopback addresses")
		}
	}

	if c.Mode != "retrieval" {
		endpoint, err := url.Parse(c.LLMEndpoint)
		if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || c.LLMModel == "" {
			return errors.New("invalid model endpoint or identity")
		}
		local := net.ParseIP(endpoint.Hostname())
		if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && local != nil && local.IsLoopback()) {
			return errors.New("model endpoint requires HTTPS or local HTTP")
		}
	}
	return nil
}

func validateLiveCases(input liveInputs) error {
	cases, scenarios := input.Cases, input.Scenarios
	if len(cases) == 0 || input.Bindings == nil || scenarios == nil {
		return errors.New("cases and explicit binding/scenario arrays are required")
	}
	if input.Config.Mode == "retrieval" {
		if len(scenarios) != 0 {
			return errors.New("retrieval mode requires empty scenarios")
		}
		for _, c := range cases {
			if c.Suite != evaluation.SuiteRetrieval || c.TopK != cases[0].TopK {
				return errors.New("retrieval batch requires retrieval cases and common K")
			}
		}
		return nil
	}

	if len(scenarios) != len(cases) {
		return errors.New("Agent scenarios must match every case")
	}
	for i, c := range cases {
		s := scenarios[i]
		if c.Suite != evaluation.SuiteAgent || *c.Filters != (evaluation.Filters{}) || s.CaseID != c.ID || s.Mode != input.Config.Mode {
			return errors.New("Agent scenario or filter mismatch")
		}
		if s.Mode == "chat" {
			if s.Input != nil {
				return errors.New("chat must not contain fixture input")
			}
		} else {
			if s.Input == nil {
				return errors.New("confirmation fixture input is required")
			}
			if _, err := service.NormalizeCreateAppealInput(*s.Input); err != nil {
				return errors.New("invalid confirmation fixture input")
			}
			raw, err := json.Marshal(s.Input)
			if err != nil || len(raw) > evaluation.MaxArgumentMatchBytes {
				return errors.New("fixture exceeds evaluation argument limit")
			}
		}
		count := strings.Count(c.Question, "$ticket_id")
		if (s.Mode == "ticket_query" && count != 1) || (s.Mode != "ticket_query" && count != 0) {
			return errors.New("invalid ticket placeholder")
		}
	}
	return nil
}
