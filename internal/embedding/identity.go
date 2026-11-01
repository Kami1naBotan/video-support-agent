package embedding

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type ModelIdentity struct {
	Model         string `json:"model"`
	Digest        string `json:"digest"`
	ServerVersion string `json:"server_version"`
	Dimensions    int    `json:"dimensions"`
	ContextLength int    `json:"context_length"`
	InputPolicy   string `json:"input_policy"`
}

func (c *Client) readIdentity(ctx context.Context) (ModelIdentity, error) {
	var zero ModelIdentity

	data, err := c.request(ctx, http.MethodGet, "/api/tags", nil)
	if err != nil {
		return zero, err
	}

	var tags struct {
		Models []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	if err := decodeResponse(data, &tags); err != nil {
		return zero, err
	}
	if tags.Models == nil {
		return zero, fmt.Errorf("%w: missing model list", ErrResponse)
	}

	matches := 0
	digest := ""
	for _, model := range tags.Models {
		if model.Name == c.model {
			matches++
			digest = model.Digest
		}
	}
	if matches != 1 || digest != c.digest {
		return zero, fmt.Errorf("%w: installed model does not match configured identity", ErrModelMismatch)
	}

	data, err = c.request(ctx, http.MethodPost, "/api/show", struct {
		Model string `json:"model"`
	}{
		Model: c.model,
	})
	if err != nil {
		return zero, err
	}

	var show struct {
		Capabilities []string `json:"capabilities"`
		ModelInfo    struct {
			EmbeddingLength *int `json:"bert.embedding_length"`
			ContextLength   *int `json:"bert.context_length"`
		} `json:"model_info"`
	}
	if err := decodeResponse(data, &show); err != nil {
		return zero, err
	}

	if show.ModelInfo.EmbeddingLength == nil || show.ModelInfo.ContextLength == nil || show.Capabilities == nil {
		return zero, fmt.Errorf("%w: missing model metadata", ErrResponse)
	}

	canEmbed := false
	for _, capability := range show.Capabilities {
		if capability == "embedding" {
			canEmbed = true
		}
	}
	if !canEmbed || *show.ModelInfo.EmbeddingLength != Dimensions || *show.ModelInfo.ContextLength != ContextLength {
		return zero, fmt.Errorf("%w: incompatible model metadata", ErrModelMismatch)
	}

	data, err = c.request(ctx, http.MethodGet, "/api/version", nil)
	if err != nil {
		return zero, err
	}

	var server struct {
		Version string `json:"version"`
	}
	if err := decodeResponse(data, &server); err != nil {
		return zero, err
	}
	if server.Version == "" || server.Version != strings.TrimSpace(server.Version) {
		return zero, fmt.Errorf("%w: invalid server version", ErrResponse)
	}

	return ModelIdentity{
		Model:         c.model,
		Digest:        digest,
		ServerVersion: server.Version,
		Dimensions:    Dimensions,
		ContextLength: ContextLength,
		InputPolicy:   InputPolicy,
	}, nil
}
