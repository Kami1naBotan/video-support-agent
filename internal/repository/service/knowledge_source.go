package service

import (
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"
)

func ValidateKnowledgeOfficialSourceURL(raw string) error {
	value := strings.TrimSpace(raw)

	if value == "" {
		return fmt.Errorf("%w: official source URL must not be blank", ErrInvalidArgument)
	}

	if utf8.RuneCountInString(value) > 2048 {
		return fmt.Errorf("%w: official source URL must not exceed 2048 characters", ErrInvalidArgument)
	}

	source, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%w: official source URL is malformed", ErrInvalidArgument)
	}

	if source.Scheme != "http" && source.Scheme != "https" {
		return fmt.Errorf("%w: official source URL must use http or https", ErrInvalidArgument)
	}

	if source.User != nil {
		return fmt.Errorf("%w: official source URL must not contain credentials", ErrInvalidArgument)
	}

	host := strings.ToLower(source.Hostname())

	switch host {
	case "www.bilibili.com":
	default:
		return fmt.Errorf("%w: official source hostname is not allowed", ErrInvalidArgument)
	}

	if strings.HasSuffix(source.Host, ":") {
		return fmt.Errorf("%w: official source URL must not contain an empty port", ErrInvalidArgument)
	}

	port := source.Port()
	if port != "" {
		standardPort := source.Scheme == "http" && port == "80" || source.Scheme == "https" && port == "443"

		if !standardPort {
			return fmt.Errorf("%w: official source URL must use its standard port", ErrInvalidArgument)
		}
	}

	return nil
}
