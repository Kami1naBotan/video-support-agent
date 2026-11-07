package evaluation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxArtifactBytes = 128 << 20

func bytesDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ReadStrictJSON(path string, destination any) (string, error) {
	target := reflect.ValueOf(destination)
	if target.Kind() != reflect.Pointer || target.IsNil() {
		return "", errors.New("artifact destination must be a nonnil pointer")
	}

	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot open artifact")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, MaxArtifactBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return "", errors.New("cannot read artifact")
	}
	if len(data) == 0 || len(data) > MaxArtifactBytes || !utf8.Valid(data) {
		return "", errors.New("artifact has invalid encoding or size")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := readJSONValue(decoder, 0)
	if err != nil {
		return "", errors.New("artifact contains invalid or duplicate JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", errors.New("artifact contains trailing data")
	}
	if err := artifactShape(value, target.Type().Elem()); err != nil {
		return "", err
	}

	temporary := reflect.New(target.Type().Elem())
	if err := json.Unmarshal(data, temporary.Interface()); err != nil {
		return "", errors.New("artifact contains an invalid field type or value")
	}
	target.Elem().Set(temporary.Elem())
	return bytesDigest(data), nil
}

func artifactShape(value any, target reflect.Type) error {
	if target == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	if target.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		return artifactShape(value, target.Elem())
	}
	if value == nil {
		return errors.New("artifact contains null in a required field")
	}
	if target == reflect.TypeFor[time.Time]() {
		if _, ok := value.(string); !ok {
			return errors.New("artifact time must be a string")
		}
		return nil
	}

	switch target.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return errors.New("artifact field must be an object")
		}
		known := make(map[string]struct{})
		for i := 0; i < target.NumField(); i++ {
			field := target.Field(i)
			if field.PkgPath != "" {
				continue
			}
			tag := strings.Split(field.Tag.Get("json"), ",")
			if tag[0] == "-" {
				continue
			}
			name := tag[0]
			if name == "" {
				name = field.Name
			}
			known[name] = struct{}{}

			optional := false
			for _, option := range tag[1:] {
				if option == "omitempty" {
					optional = true
				}
			}
			child, exists := object[name]
			if !exists {
				if optional {
					continue
				}
				return errors.New("artifact is missing a required field")
			}
			if err := artifactShape(child, field.Type); err != nil {
				return err
			}
		}
		for name := range object {
			if _, exists := known[name]; !exists {
				return errors.New("artifact contains an unknown field")
			}
		}

	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			return errors.New("artifact field must be an array")
		}
		for _, item := range items {
			if err := artifactShape(item, target.Elem()); err != nil {
				return err
			}
		}

	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok || target.Key().Kind() != reflect.String {
			return errors.New("artifact field must be a string-keyed object")
		}
		for _, child := range object {
			if err := artifactShape(child, target.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func WriteNewJSON(path string, value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", errors.New("cannot encode artifact")
	}
	data = append(data, '\n')
	if len(data) > MaxArtifactBytes {
		return "", errors.New("artifact exceeds size limit")
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", errors.New("cannot create new artifact")
	}

	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return "", errors.New("artifact write failed; incomplete file retained")
	}
	return bytesDigest(data), nil
}
