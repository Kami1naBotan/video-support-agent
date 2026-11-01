package embedding

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func contractTexts(count int, text string) []string {
	result := make([]string, count)
	for i := range result {
		result[i] = text
	}
	return result
}

func contractVector(first float64) []float64 {
	vector := make([]float64, Dimensions)
	vector[0] = first
	return vector
}

func TestPrepareInputsPreservesTextAndOwnership(t *testing.T) {
	input := []string{
		" \t投稿规则：第一条。\r\n",
		"账号规则\n第二行\u3000",
		" \t投稿规则：第一条。\r\n",
	}
	want := append([]string(nil), input...)

	got, err := PrepareInputs(input)
	if err != nil {
		t.Fatalf("PrepareInputs: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("text, order or duplicate input was changed")
	}
	if !reflect.DeepEqual(input, want) {
		t.Fatal("input was modified")
	}

	got[0] = "changed output"
	if input[0] != want[0] {
		t.Fatal("output slice aliases input slice")
	}
	input[1] = "changed input"
	if got[1] != want[1] {
		t.Fatal("input mutation changed prepared output")
	}
}

func TestPrepareInputsBoundaries(t *testing.T) {
	exactASCII := strings.Repeat("a", MaxTextBytes)
	exactChinese := strings.Repeat("中", MaxTextBytes/3) +
		strings.Repeat("a", MaxTextBytes%3)

	exactBatch := contractTexts(4, exactASCII)
	overBatch := append(append([]string(nil), exactBatch...), "a")

	tests := []struct {
		name string
		in   []string
		want error
	}{
		{"nil", nil, ErrInput},
		{"empty_batch", []string{}, ErrInput},
		{"empty_text", []string{""}, ErrInput},
		{"unicode_whitespace", []string{" \t\r\n\u3000\u00a0"}, ErrInput},
		{"invalid_utf8", []string{string([]byte{0xff, 0xfe})}, ErrInput},
		{"second_item_invalid", []string{"正常正文", ""}, ErrInput},
		{"exact_item_count", contractTexts(MaxBatchItems, "规则"), nil},
		{"over_item_count", contractTexts(MaxBatchItems+1, "规则"), ErrInputTooLarge},
		{"exact_ascii_bytes", []string{exactASCII}, nil},
		{"over_ascii_bytes", []string{exactASCII + "a"}, ErrInputTooLarge},
		{"exact_chinese_bytes", []string{exactChinese}, nil},
		{"over_chinese_bytes", []string{exactChinese + "a"}, ErrInputTooLarge},
		{"exact_batch_bytes", exactBatch, nil},
		{"over_batch_bytes", overBatch, ErrInputTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PrepareInputs(tt.in)
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("error = %v, want %v", err, tt.want)
				}
				if got != nil {
					t.Fatal("invalid batch returned partial inputs")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.in) {
				t.Fatal("accepted input was changed")
			}
		})
	}
}

func TestPrepareInputsErrorsDoNotExposeText(t *testing.T) {
	const marker = "PRIVATE_RULE_CONTENT_92741"
	tests := []struct {
		name string
		in   []string
		want error
	}{
		{
			"invalid_utf8",
			[]string{marker + string([]byte{0xff})},
			ErrInput,
		},
		{
			"oversized_text",
			[]string{marker + strings.Repeat("x", MaxTextBytes)},
			ErrInputTooLarge,
		},
		{
			"later_invalid_item",
			[]string{marker, ""},
			ErrInput,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PrepareInputs(tt.in)
			if !errors.Is(err, tt.want) || got != nil {
				t.Fatalf("result = %v, error = %v", got, err)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatal("error exposed input text")
			}
		})
	}
}

func TestValidateVectorsPreservesValuesAndOwnership(t *testing.T) {
	first := contractVector(0.1)
	first[1] = -2.5
	second := contractVector(3.25)
	raw := [][]float64{first, second}

	got, err := ValidateVectors(raw, 2)
	if err != nil {
		t.Fatalf("ValidateVectors: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("vector count = %d, want 2", len(got))
	}
	for i := range raw {
		if len(got[i]) != Dimensions {
			t.Fatalf("vector %d dimensions = %d", i, len(got[i]))
		}
		for j := range raw[i] {
			if got[i][j] != float32(raw[i][j]) {
				t.Fatalf("value differs at vector %d component %d", i, j)
			}
		}
	}
	if raw[0][0] != 0.1 || raw[0][1] != -2.5 || raw[1][0] != 3.25 {
		t.Fatal("raw vectors were modified")
	}

	raw[0][0] = 99
	if got[0][0] != float32(0.1) {
		t.Fatal("raw mutation changed converted result")
	}
	got[0][1] = 77
	if raw[0][1] != -2.5 {
		t.Fatal("result mutation changed raw vector")
	}
	if got[1][1] != 0 {
		t.Fatal("output vectors share storage")
	}

	// 即使原始批次重复引用同一个切片，结果也应各自独立。
	shared := contractVector(1)
	copies, err := ValidateVectors([][]float64{shared, shared}, 2)
	if err != nil {
		t.Fatal(err)
	}
	copies[0][0] = 5
	if copies[1][0] != 1 || shared[0] != 1 {
		t.Fatal("repeated source vectors produced shared output storage")
	}
}

func TestValidateVectorsAcceptsNumericBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		value float64
	}{
		{"positive_max", float64(math.MaxFloat32)},
		{"negative_max", -float64(math.MaxFloat32)},
		{"positive_smallest", float64(math.SmallestNonzeroFloat32)},
		{"negative_smallest", -float64(math.SmallestNonzeroFloat32)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := contractVector(tt.value)
			raw[1] = math.SmallestNonzeroFloat64
			got, err := ValidateVectors([][]float64{raw}, 1)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 || len(got[0]) != Dimensions {
				t.Fatal("incorrect result shape")
			}
			if got[0][0] != float32(tt.value) || got[0][0] == 0 {
				t.Fatal("representable boundary value was lost")
			}
			if got[0][1] != 0 {
				t.Fatal("expected tiny component to underflow to zero")
			}
		})
	}

	t.Run("exact_max_batch", func(t *testing.T) {
		raw := make([][]float64, MaxBatchItems)
		for i := range raw {
			raw[i] = contractVector(float64(i + 1))
		}
		got, err := ValidateVectors(raw, MaxBatchItems)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != MaxBatchItems {
			t.Fatal("wrong batch size")
		}
		for i := range got {
			if len(got[i]) != Dimensions || got[i][0] != float32(i+1) {
				t.Fatalf("wrong shape or order at vector %d", i)
			}
		}
	})
}

func TestValidateVectorsRejectsInvalidBatch(t *testing.T) {
	withBadComponent := func(value float64) [][]float64 {
		vector := contractVector(1)
		vector[Dimensions-1] = value
		return [][]float64{vector}
	}

	tests := []struct {
		name     string
		raw      [][]float64
		expected int
		want     error
	}{
		{"negative_expected", nil, -1, ErrInput},
		{"zero_expected", nil, 0, ErrInput},
		{"over_expected", nil, MaxBatchItems + 1, ErrInput},
		{"missing_vector", nil, 1, ErrResponse},
		{"too_few_vectors", [][]float64{contractVector(1)}, 2, ErrResponse},
		{"too_many_vectors", [][]float64{contractVector(1), contractVector(2)}, 1, ErrResponse},
		{"nil_vector", [][]float64{nil}, 1, ErrResponse},
		{"short_vector", [][]float64{make([]float64, Dimensions-1)}, 1, ErrResponse},
		{"long_vector", [][]float64{make([]float64, Dimensions+1)}, 1, ErrResponse},
		{"zero_vector", [][]float64{contractVector(0)}, 1, ErrResponse},
		{"underflow_to_zero", [][]float64{contractVector(math.SmallestNonzeroFloat64)}, 1, ErrResponse},
		{"nan", withBadComponent(math.NaN()), 1, ErrResponse},
		{"positive_infinity", withBadComponent(math.Inf(1)), 1, ErrResponse},
		{"negative_infinity", withBadComponent(math.Inf(-1)), 1, ErrResponse},
		{
			"positive_overflow",
			withBadComponent(math.Nextafter(float64(math.MaxFloat32), math.Inf(1))),
			1, ErrResponse,
		},
		{
			"negative_overflow",
			withBadComponent(math.Nextafter(-float64(math.MaxFloat32), math.Inf(-1))),
			1, ErrResponse,
		},
		{
			"valid_first_invalid_second",
			[][]float64{contractVector(1), contractVector(math.NaN())},
			2, ErrResponse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateVectors(tt.raw, tt.expected)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if got != nil {
				t.Fatal("invalid batch returned partial vectors")
			}
		})
	}
}
