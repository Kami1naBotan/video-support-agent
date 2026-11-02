package rulesource

import (
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"
)

func ValidateOfficialURL(raw string) error {
	value := strings.TrimSpace(raw)

	if value == "" {
		return errors.New("official source URL must not be blank")
	}
	if utf8.RuneCountInString(value) > 2048 {
		return errors.New("official source URL must not exceed 2048 characters")
	}

	source, err := url.Parse(value)
	if err != nil {
		return errors.New("official source URL is malformed")
	}
	if source.Scheme != "http" && source.Scheme != "https" {
		return errors.New("official source URL must use http or https")
	}
	if source.User != nil {
		return errors.New("official source URL must not contain credentials")
	}

	switch strings.ToLower(source.Hostname()) {
	case "www.bilibili.com":
	default:
		return errors.New("official source hostname is not allowed")
	}

	if strings.HasSuffix(source.Host, ":") {
		return errors.New("official source URL must not contain an empty port")
	}

	port := source.Port()
	if port != "" {
		standardPort := source.Scheme == "http" && port == "80" || source.Scheme == "https" && port == "443"
		if !standardPort {
			return errors.New("official source URL must use its standard port")
		}
	}

	return nil
}
