package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"video-support-agent/internal/service"
)

// 只替代仓储边界，不连接数据库。
// 基础KnowledgeStore方法复用已有managementStore；意外调用会panic。
type withdrawalStore struct {
	managementStore

	withdraw func(
		context.Context,
		uint64,
		service.WithdrawKnowledgeVersionInput,
	) (*service.KnowledgeWithdrawalResult, error)
}

var _ service.KnowledgeStore = (*withdrawalStore)(nil)
var _ service.KnowledgeVersionWithdrawer = (*withdrawalStore)(nil)

func (s *withdrawalStore) WithdrawVersion(
	ctx context.Context,
	operatorID uint64,
	input service.WithdrawKnowledgeVersionInput,
) (*service.KnowledgeWithdrawalResult, error) {
	if s.withdraw == nil {
		panic("unexpected withdrawal storage call")
	}
	return s.withdraw(ctx, operatorID, input)
}

func TestKnowledgeWithdrawalServiceRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name       string
		operatorID uint64
		input      service.WithdrawKnowledgeVersionInput
		want       error
	}{
		{
			name:       "missing_operator",
			operatorID: 0,
			input: service.WithdrawKnowledgeVersionInput{
				DocumentID:                  11,
				VersionID:                   21,
				ExpectedPublicationRevision: 7,
			},
			want: service.ErrUnauthorized,
		},
		{
			name:       "zero_document",
			operatorID: 31,
			input: service.WithdrawKnowledgeVersionInput{
				DocumentID:                  0,
				VersionID:                   21,
				ExpectedPublicationRevision: 7,
			},
			want: service.ErrInvalidArgument,
		},
		{
			name:       "zero_version",
			operatorID: 31,
			input: service.WithdrawKnowledgeVersionInput{
				DocumentID:                  11,
				VersionID:                   0,
				ExpectedPublicationRevision: 7,
			},
			want: service.ErrInvalidArgument,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			store := &withdrawalStore{
				withdraw: func(
					context.Context,
					uint64,
					service.WithdrawKnowledgeVersionInput,
				) (*service.KnowledgeWithdrawalResult, error) {
					calls++
					return nil, errors.New("invalid input reached storage")
				},
			}

			result, err := service.NewKnowledgeService(store).
				WithdrawVersion(context.Background(), tc.operatorID, tc.input)

			if result != nil || !errors.Is(err, tc.want) {
				t.Fatalf("result=%+v err=%v, want nil and %v",
					result, err, tc.want)
			}
			if calls != 0 {
				t.Fatalf("storage calls=%d, want 0", calls)
			}
		})
	}
}

func TestKnowledgeWithdrawalServiceForwardsInputAndResult(t *testing.T) {
	// 包含零值及超过float64精确整数范围的值，确保uint64原样传递。
	// 这里只验证服务边界；不表示真实数据库中这些修订均可成功下线。
	revisions := []uint64{0, 7, 9007199254740993}

	for _, revision := range revisions {
		t.Run(fmt.Sprintf("revision_%d", revision), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			input := service.WithdrawKnowledgeVersionInput{
				DocumentID:                  11,
				VersionID:                   21,
				ExpectedPublicationRevision: revision,
			}
			before := input
			withdrawnAt := time.Date(
				2026, 10, 28, 2, 3, 4, 123000000, time.UTC,
			)
			saved := &service.KnowledgeWithdrawalResult{
				DocumentID:          11,
				VersionID:           21,
				PublicationRevision: revision + 1,
				WithdrawnAt:         withdrawnAt,
			}

			calls := 0
			store := &withdrawalStore{
				withdraw: func(
					gotCtx context.Context,
					operatorID uint64,
					got service.WithdrawKnowledgeVersionInput,
				) (*service.KnowledgeWithdrawalResult, error) {
					calls++
					if gotCtx != ctx {
						t.Fatal("request context was not forwarded")
					}
					if operatorID != 31 || got != input {
						t.Fatalf("operator=%d input=%+v, want 31 and %+v",
							operatorID, got, input)
					}
					return saved, nil
				},
			}

			result, err := service.NewKnowledgeService(store).
				WithdrawVersion(ctx, 31, input)
			if err != nil {
				t.Fatal(err)
			}
			if result == nil {
				t.Fatal("successful storage result was lost")
			}
			if result.DocumentID != saved.DocumentID ||
				result.VersionID != saved.VersionID ||
				result.PublicationRevision != saved.PublicationRevision ||
				!result.WithdrawnAt.Equal(saved.WithdrawnAt) {
				t.Fatalf("result=%+v, want %+v", result, saved)
			}
			if calls != 1 {
				t.Fatalf("storage calls=%d, want 1", calls)
			}
			if input != before {
				t.Fatalf("caller input changed: got=%+v want=%+v", input, before)
			}
		})
	}
}

func TestKnowledgeWithdrawalServicePreservesErrors(t *testing.T) {
	storageFailure := errors.New("test-only storage failure")

	tests := []struct {
		name string
		want error
	}{
		{"document_not_found", service.ErrKnowledgeDocumentNotFound},
		{"version_not_found", service.ErrKnowledgeVersionNotFound},
		{"not_withdrawable", service.ErrKnowledgeVersionNotWithdrawable},
		{"publication_conflict", service.ErrKnowledgePublicationConflict},
		{"deadline", context.DeadlineExceeded},
		{"canceled", context.Canceled},
		{"storage_failure", storageFailure},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			store := &withdrawalStore{
				withdraw: func(
					context.Context,
					uint64,
					service.WithdrawKnowledgeVersionInput,
				) (*service.KnowledgeWithdrawalResult, error) {
					calls++
					// 即使仓储同时返回结果和错误，也不能向调用方报告成功。
					return &service.KnowledgeWithdrawalResult{
						DocumentID:          11,
						VersionID:           21,
						PublicationRevision: 8,
					}, fmt.Errorf("storage boundary: %w", tc.want)
				},
			}

			result, err := service.NewKnowledgeService(store).
				WithdrawVersion(context.Background(), 31,
					service.WithdrawKnowledgeVersionInput{
						DocumentID:                  11,
						VersionID:                   21,
						ExpectedPublicationRevision: 7,
					})

			if result != nil {
				t.Fatalf("error returned with a successful result: %+v", result)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v, want errors.Is(err, %v)", err, tc.want)
			}
			if calls != 1 {
				t.Fatalf("storage calls=%d, want 1", calls)
			}
		})
	}
}

func TestKnowledgeWithdrawalServiceRejectsMissingCapability(t *testing.T) {
	// managementStore实现基础KnowledgeStore，但没有WithdrawVersion能力。
	svc := service.NewKnowledgeService(&managementStore{})

	result, err := svc.WithdrawVersion(
		context.Background(),
		31,
		service.WithdrawKnowledgeVersionInput{
			DocumentID:                  11,
			VersionID:                   21,
			ExpectedPublicationRevision: 7,
		},
	)

	if result != nil || err == nil {
		t.Fatalf("result=%+v err=%v, want nil result and configuration error",
			result, err)
	}
	if errors.Is(err, service.ErrInvalidArgument) ||
		errors.Is(err, service.ErrUnauthorized) ||
		errors.Is(err, service.ErrKnowledgePublicationConflict) ||
		errors.Is(err, service.ErrKnowledgeVersionNotWithdrawable) {
		t.Fatalf("configuration failure misclassified as client/domain error: %v", err)
	}
}

func TestKnowledgeWithdrawalServiceRejectsEmptySuccess(t *testing.T) {
	calls := 0
	store := &withdrawalStore{
		withdraw: func(
			context.Context,
			uint64,
			service.WithdrawKnowledgeVersionInput,
		) (*service.KnowledgeWithdrawalResult, error) {
			calls++
			return nil, nil
		},
	}

	result, err := service.NewKnowledgeService(store).
		WithdrawVersion(context.Background(), 31,
			service.WithdrawKnowledgeVersionInput{
				DocumentID:                  11,
				VersionID:                   21,
				ExpectedPublicationRevision: 7,
			})

	if result != nil || err == nil {
		t.Fatalf("result=%+v err=%v, want nil result and internal error",
			result, err)
	}
	if calls != 1 {
		t.Fatalf("storage calls=%d, want 1", calls)
	}
	if errors.Is(err, service.ErrKnowledgePublicationConflict) ||
		errors.Is(err, service.ErrKnowledgeVersionNotWithdrawable) {
		t.Fatalf("empty storage result misclassified as business conflict: %v", err)
	}
}
