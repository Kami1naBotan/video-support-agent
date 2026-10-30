package config

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var elasticsearchAliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,254}$`)

func ValidateElasticsearchConfig(cfg ElasticsearchConfig) error {
	if cfg.URL == "" || cfg.URL != strings.TrimSpace(cfg.URL) {
		return errors.New("ES_URL must be non-empty without surrounding whitespace")
	}

	endpoint, err := url.Parse(cfg.URL)
	if err != nil {
		return errors.New("ES_URL must be a valid HTTP or HTTPS root URL")
	}

	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return errors.New("ES_URL must use HTTP or HTTPS")
	}

	if endpoint.Hostname() == "" || endpoint.Opaque != "" {
		return errors.New("ES_URL must include a valid host")
	}

	if endpoint.User != nil {
		return errors.New("ES_URL must not contain credentials")
	}

	if endpoint.RawQuery != "" ||
		endpoint.ForceQuery || strings.Contains(cfg.URL, "#") {
		return errors.New("ES_URL must not contain a query or fragment")
	}

	if endpoint.Path != "" && endpoint.Path != "/" {
		return errors.New("ES_URL must be a root URL without a path prefix")
	}

	if endpoint.RawPath != "" {
		return errors.New("ES_URL must not contain an encoded path")
	}

	port := endpoint.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return errors.New("ES_URL port must be between 1 and 65535")
		}
	} else if strings.HasSuffix(endpoint.Host, ":") {
		return errors.New("ES_URL must not contain an empty port")
	}

	if !elasticsearchAliasPattern.MatchString(cfg.IndexAlias) {
		return errors.New("ES_INDEX_ALIAS must contain 1 to 255 lowercase letters, digits, underscores or hyphens, starting with a letter or digit")
	}

	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil || timeout <= 0 {
		return errors.New("ES_TIMEOUT must be a positive duration")
	}

	return nil
}
