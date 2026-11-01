package milvus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	"video-support-agent/internal/embedding"
)

var targetPattern = regexp.MustCompile(`^rule_chunks_vec_v1_[0-9a-f]{32}$`)

type fieldSpec struct {
	Name    string
	Type    string
	Primary bool
	Params  map[string]string
}

func collectionFields() []fieldSpec {
	fields := make([]fieldSpec, 0, 11)
	for _, name := range []string{
		"chunk_id", "document_id", "version_id",
		"article_id", "category", "publication_revision",
	} {
		fields = append(fields, fieldSpec{
			Name:    name,
			Type:    "VarChar",
			Primary: name == "chunk_id",
			Params:  map[string]string{"max_length": "20"},
		})
	}
	return append(fields,
		fieldSpec{
			Name: "version", Type: "VarChar",
			Params: map[string]string{"max_length": "256"},
		},
		fieldSpec{
			Name: "embedding", Type: "FloatVector",
			Params: map[string]string{"dim": strconv.Itoa(embedding.Dimensions)},
		},
		fieldSpec{Name: "effective_at", Type: "Int64"},
		fieldSpec{Name: "has_expires_at", Type: "Bool"},
		fieldSpec{Name: "expires_at", Type: "Int64"},
	)
}

func NewCollectionName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("%w: cannot generate collection name", ErrInput)
	}
	return "rule_chunks_vec_v1_" + hex.EncodeToString(random[:]), nil
}

func contractDescription(identity embedding.ModelIdentity) (string, error) {
	if err := checkIdentity(identity, identity.Digest); err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Version  int                     `json:"contract_version"`
		Identity embedding.ModelIdentity `json:"identity"`
	}{
		Version:  1,
		Identity: identity,
	})
	if err != nil {
		return "", fmt.Errorf("%w: cannot encode model contract", ErrInput)
	}
	return string(data), nil
}

func (c *Client) ensureUnused(ctx context.Context, name string) error {
	for _, path := range []string{"collections/list", "aliases/list"} {
		data, err := c.request(ctx, path, nil, false)
		if err != nil {
			return err
		}
		var names []string
		if json.Unmarshal(data, &names) != nil || names == nil {
			return fmt.Errorf("%w: invalid resource list", ErrResponse)
		}
		for _, existing := range names {
			if existing == name {
				return fmt.Errorf("%w: target already exists", ErrInput)
			}
		}
	}
	return nil
}

func (c *Client) createCollection(ctx context.Context, name string, description string) error {
	fields := make([]map[string]any, 0, 11)
	for _, field := range collectionFields() {
		item := map[string]any{
			"fieldName": field.Name,
			"dataType":  field.Type,
			"isPrimary": field.Primary,
			"nullable":  false,
		}
		if len(field.Params) != 0 {
			item["elementTypeParams"] = field.Params
		}
		fields = append(fields, item)
	}

	_, err := c.request(ctx, "collections/create", map[string]any{
		"collectionName": name,
		"description":    description,
		"schema": map[string]any{
			"autoId":              false,
			"enabledDynamicField": false,
			"fields":              fields,
		},
		"params": map[string]any{
			"consistencyLevel": "Strong",
			"shardsNum":        1,
		},
	}, true)
	return err
}

func (c *Client) VerifyCollectionContract(ctx context.Context, name string, identity embedding.ModelIdentity) (int64, error) {
	expectedDescription, err := contractDescription(identity)
	if err != nil {
		return 0, err
	}
	data, err := c.request(ctx, "collections/describe", map[string]any{"collectionName": name}, false)
	if err != nil {
		return 0, err
	}

	var actual struct {
		CollectionName     string `json:"collectionName"`
		CollectionID       int64  `json:"collectionID"`
		Description        string `json:"description"`
		AutoID             *bool  `json:"autoId"`
		EnableDynamicField *bool  `json:"enableDynamicField"`
		ConsistencyLevel   string `json:"consistencyLevel"`
		ShardsNum          int    `json:"shardsNum"`
		Fields             []struct {
			Name       string `json:"name"`
			Type       string `json:"type"`
			PrimaryKey *bool  `json:"primaryKey"`
			Nullable   *bool  `json:"nullable"`
			AutoID     *bool  `json:"autoId"`
			Params     []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"params"`
		} `json:"fields"`
	}
	if json.Unmarshal(data, &actual) != nil {
		return 0, fmt.Errorf("%w: invalid collection description", ErrResponse)
	}
	if actual.CollectionName != name || actual.CollectionID <= 0 || actual.Description != expectedDescription || !boolEquals(actual.AutoID, false) || !boolEquals(actual.EnableDynamicField, false) || actual.ConsistencyLevel != "Strong" || actual.ShardsNum != 1 ||
		len(actual.Fields) != 11 {
		return 0, fmt.Errorf("%w: collection contract differs", ErrResponse)
	}

	for _, expected := range collectionFields() {
		matches := 0
		for _, field := range actual.Fields {
			if field.Name != expected.Name {
				continue
			}
			matches++
			if field.Type != expected.Type || !boolEquals(field.PrimaryKey, expected.Primary) || !boolEquals(field.Nullable, false) {
				return 0, fmt.Errorf("%w: field contract differs: %s", ErrResponse, expected.Name)
			}

			if expected.Primary {
				if !boolEquals(field.AutoID, false) {
					return 0, fmt.Errorf("%w: primary field autoId differs: %s", ErrResponse, expected.Name)
				}
			} else if field.AutoID != nil && *field.AutoID {
				return 0, fmt.Errorf("%w: non-primary field autoId differs: %s", ErrResponse, expected.Name)
			}
			for key, value := range expected.Params {
				found := 0
				for _, parameter := range field.Params {
					if parameter.Key == key {
						found++
						if parameter.Value != value {
							return 0, fmt.Errorf("%w: field parameter differs: %s", ErrResponse, expected.Name)
						}
					}
				}
				if found != 1 {
					return 0, fmt.Errorf("%w: missing or duplicate field parameter", ErrResponse)
				}
			}
		}
		if matches != 1 {
			return 0, fmt.Errorf("%w: missing or duplicate field: %s", ErrResponse, expected.Name)
		}
	}
	return actual.CollectionID, nil
}

func boolEquals(value *bool, expected bool) bool {
	return value != nil && *value == expected
}
