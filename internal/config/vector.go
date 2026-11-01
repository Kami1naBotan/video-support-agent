package config

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	vectorNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,254}$`)
	modelDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func ValidateEmbeddingConfig(cfg EmbeddingConfig) error {
	if err := validateVectorURL("EMBEDDING_URL", cfg.URL); err != nil {
		return err
	}

	if cfg.Model != "bge-m3:567m" {
		return errors.New("EMBEDDING_MODEL must be bge-m3:567m")
	}

	if !modelDigestPattern.MatchString(cfg.ModelDigest) {
		return errors.New("EMBEDDING_MODEL_DIGEST must be 64 lowercase hexadecimal characters")
	}

	if cfg.Dimensions != "1024" {
		return errors.New("EMBEDDING_DIMENSIONS must be 1024 for the selected model")
	}

	if cfg.ContextLength != "8192" {
		return errors.New("EMBEDDING_CONTEXT_LENGTH must be 8192 for the selected model")
	}

	return validateVectorTimeout("EMBEDDING_TIMEOUT", cfg.Timeout)
}

func ValidateMilvusConfig(cfg MilvusConfig) error {
	if err := validateVectorURL("MILVUS_URL", cfg.URL); err != nil {
		return err
	}

	if !vectorNamePattern.MatchString(cfg.Database) {
		return errors.New("MILVUS_DATABASE must be a valid identifier of at most 255 ASCII characters")
	}

	if !vectorNamePattern.MatchString(cfg.CollectionAlias) {
		return errors.New("MILVUS_COLLECTION_ALIAS must be a valid identifier of at most 255 ASCII characters")
	}

	return validateVectorTimeout("MILVUS_TIMEOUT", cfg.Timeout)
}

func validateVectorURL(name, raw string) error {
	invalid := func() error {
		return errors.New(name + " must be an HTTP or HTTPS root URL without credentials, query or fragment")
	}

	if raw == "" || raw != strings.TrimSpace(raw) {
		return invalid()
	}

	endpoint, err := url.Parse(raw)
	if err != nil {
		return invalid()
	}

	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return invalid()
	}

	if endpoint.Hostname() == "" || endpoint.Opaque != "" || endpoint.User != nil {
		return invalid()
	}

	if endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(raw, "#") {
		return invalid()
	}

	if endpoint.Path != "" && endpoint.Path != "/" {
		return invalid()
	}

	if endpoint.RawPath != "" {
		return invalid()
	}

	if port := endpoint.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return invalid()
		}
	} else if strings.HasSuffix(endpoint.Host, ":") {
		return invalid()
	}

	return nil
}

func validateVectorTimeout(name, raw string) error {
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		return errors.New(name + " must be a positive duration")
	}
	return nil
}
