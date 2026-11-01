package embedding

import (
	"context"
	"fmt"
	"net/http"
)

type BatchResult struct {
	Identity ModelIdentity
	Vectors  [][]float32
}

func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	result, err := c.EmbedWithIdentity(ctx, texts)
	if err != nil {
		return nil, err
	}
	return result.Vectors, nil
}

func (c *Client) EmbedWithIdentity(ctx context.Context, texts []string) (BatchResult, error) {
	var zero BatchResult

	if err := ctx.Err(); err != nil {
		return zero, err
	}

	prepared, err := PrepareInputs(texts)
	if err != nil {
		return zero, err
	}

	opCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	before, err := c.readIdentity(opCtx)
	if err != nil {
		return zero, err
	}

	request := struct {
		Model     string   `json:"model"`
		Input     []string `json:"input"`
		Truncate  bool     `json:"truncate"`
		KeepAlive string   `json:"keep_alive"`
		Options   struct {
			NumCtx int `json:"num_ctx"`
		} `json:"options"`
	}{
		Model:     c.model,
		Input:     prepared,
		Truncate:  false,
		KeepAlive: "5m",
	}
	request.Options.NumCtx = ContextLength

	data, err := c.request(opCtx, http.MethodPost, "/api/embed", request)
	if err != nil {
		return zero, err
	}

	var response struct {
		Model      string       `json:"model"`
		Embeddings [][]*float64 `json:"embeddings"`
	}
	if err := decodeResponse(data, &response); err != nil {
		return zero, err
	}
	if response.Model != c.model {
		return zero, fmt.Errorf("%w: embedding response model differs", ErrModelMismatch)
	}
	if len(response.Embeddings) != len(prepared) {
		return zero, fmt.Errorf("%w: embedding count differs from input count", ErrResponse)
	}

	raw := make([][]float64, len(response.Embeddings))
	for i, vector := range response.Embeddings {
		if len(vector) != Dimensions {
			return zero, fmt.Errorf("%w: vector %d has invalid dimensions", ErrResponse, i)
		}

		raw[i] = make([]float64, Dimensions)
		for j, value := range vector {
			if value == nil {
				return zero, fmt.Errorf("%w: vector %d component %d is null", ErrResponse, i, j)
			}
			raw[i][j] = *value
		}
	}

	vectors, err := ValidateVectors(raw, len(prepared))
	if err != nil {
		return zero, err
	}

	after, err := c.readIdentity(opCtx)
	if err != nil {
		return zero, err
	}
	if before != after {
		return zero, fmt.Errorf("%w: identity changed during embedding operation", ErrModelMismatch)
	}
	if err := opCtx.Err(); err != nil {
		return zero, err
	}

	return BatchResult{
		Identity: after,
		Vectors:  vectors,
	}, nil
}
