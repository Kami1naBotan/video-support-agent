package config

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type KnowledgeWorkerSettings struct {
	SourceGroup        string
	RetryGroup         string
	ExpectedServerUUID string
	GenerationLimit    uint64
	ESDefinitionPath   string
}

func LoadKnowledgeWorkerSettings() (KnowledgeWorkerSettings, error) {
	settings := KnowledgeWorkerSettings{
		SourceGroup:        getEnv("KNOWLEDGE_SOURCE_GROUP", "knowledge-index-worker-v1"),
		RetryGroup:         getEnv("KNOWLEDGE_RETRY_GROUP", "knowledge-index-retry-worker-v1"),
		ExpectedServerUUID: os.Getenv("KNOWLEDGE_SOURCE_SERVER_UUID"),
		ESDefinitionPath:   getEnv("KNOWLEDGE_ES_DEFINITION", "deploy/elasticsearch/rule-chunks-v1.json"),
	}

	groupPattern := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	uuidPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if !groupPattern.MatchString(settings.SourceGroup) ||
		!groupPattern.MatchString(settings.RetryGroup) ||
		settings.SourceGroup == settings.RetryGroup ||
		!uuidPattern.MatchString(settings.ExpectedServerUUID) ||
		strings.TrimSpace(settings.ESDefinitionPath) == "" ||
		settings.ESDefinitionPath != strings.TrimSpace(settings.ESDefinitionPath) {
		return KnowledgeWorkerSettings{}, errors.New("knowledge_worker_config_invalid")
	}
	rawLimit := os.Getenv("KNOWLEDGE_GENERATION_LIMIT")
	limit, err := strconv.ParseUint(rawLimit, 10, 64)
	if err != nil || limit == 0 || strconv.FormatUint(limit, 10) != rawLimit {
		return KnowledgeWorkerSettings{}, errors.New("knowledge_generation_limit_invalid")
	}
	settings.GenerationLimit = limit

	return settings, nil
}
