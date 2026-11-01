package embedding

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

func PrepareInputs(texts []string) ([]string, error) {
	if len(texts) == 0 {
		return nil, fmt.Errorf("%w: batch is empty", ErrInput)
	}
	if len(texts) > MaxBatchItems {
		return nil, fmt.Errorf("%w: batch item count exceeds %d", ErrInputTooLarge, MaxBatchItems)
	}

	totalBytes := 0

	for i, text := range texts {
		if len(text) > MaxTextBytes {
			return nil, fmt.Errorf("%w: item %d exceeds %d bytes", ErrInputTooLarge, i, MaxTextBytes)
		}

		if !utf8.ValidString(text) {
			return nil, fmt.Errorf("%w: item %d is not valid UTF-8", ErrInput, i)
		}

		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("%w: item %d is blank", ErrInput, i)
		}

		if len(text) > MaxBatchTextBytes-totalBytes {
			return nil, fmt.Errorf("%w: batch text exceeds %d bytes at item %d", ErrInputTooLarge, MaxBatchTextBytes, i)
		}

		totalBytes += len(text)
	}

	prepared := make([]string, len(texts))
	copy(prepared, texts)
	return prepared, nil
}

func ValidateVectors(raw [][]float64, expectedCount int) ([][]float32, error) {
	if expectedCount < 1 || expectedCount > MaxBatchItems {
		return nil, fmt.Errorf("%w: expected count must be between 1 and %d", ErrInput, MaxBatchItems)
	}

	if len(raw) != expectedCount {
		return nil, fmt.Errorf("%w: expected %d vectors, received %d", ErrResponse, expectedCount, len(raw))
	}

	vectors := make([][]float32, expectedCount)

	for i, vector := range raw {
		if len(vector) != Dimensions {
			return nil, fmt.Errorf("%w: vector %d has %d dimensions, expected %d", ErrResponse, i, len(vector), Dimensions)
		}

		converted := make([]float32, Dimensions)
		hasNonzero := false

		for j, value := range vector {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, fmt.Errorf("%w: vector %d component %d is not finite", ErrResponse, i, j)
			}

			if value > math.MaxFloat32 || value < -math.MaxFloat32 {
				return nil, fmt.Errorf("%w: vector %d component %d exceeds float32 range", ErrResponse, i, j)
			}

			component := float32(value)
			converted[j] = component

			if component != 0 {
				hasNonzero = true
			}
		}

		if !hasNonzero {
			return nil, fmt.Errorf("%w: vector %d is zero after float32 conversion", ErrResponse, i)
		}

		vectors[i] = converted
	}

	return vectors, nil
}
