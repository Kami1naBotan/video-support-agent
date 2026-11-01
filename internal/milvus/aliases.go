package milvus

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

var aliasTargetPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,254}$`)

type PublicationState string

const (
	PublicationNotAttempted PublicationState = "not_attempted"
	PublicationApplied      PublicationState = "applied"
	PublicationOldObserved  PublicationState = "old_observed"
	PublicationUnknown      PublicationState = "unknown"
)

type PublicationResult struct {
	State                PublicationState `json:"state"`
	ExpectedOld          string           `json:"expected_old"`
	NewTarget            string           `json:"new_target"`
	ObservedTarget       string           `json:"observed_target"`
	ObservationSucceeded bool             `json:"observation_succeeded"`
	RequestAcknowledged  bool             `json:"request_acknowledged"`
	RequestError         string           `json:"request_error,omitempty"`
	ObservationError     string           `json:"observation_error,omitempty"`
}

// 空字符串表示该Alias当前不存在。
// 先列Alias，避免把任意业务错误误当作“不存在”。
func (c *Client) GetAliasTarget(ctx context.Context) (string, error) {
	data, err := c.request(ctx, "aliases/list", nil, false)
	if err != nil {
		return "", err
	}
	var names []string
	if json.Unmarshal(data, &names) != nil || names == nil {
		return "", fmt.Errorf("%w: invalid alias list", ErrResponse)
	}

	matches := 0
	for _, name := range names {
		if name == c.alias {
			matches++
		}
	}
	if matches == 0 {
		return "", nil
	}
	if matches != 1 {
		return "", fmt.Errorf("%w: duplicate alias listing", ErrResponse)
	}

	data, err = c.request(ctx, "aliases/describe", map[string]any{
		"aliasName": c.alias,
	}, false)
	if err != nil {
		return "", err
	}
	var alias struct {
		AliasName      string `json:"aliasName"`
		CollectionName string `json:"collectionName"`
		DBName         string `json:"dbName"`
	}
	if json.Unmarshal(data, &alias) != nil ||
		alias.AliasName != c.alias ||
		alias.DBName != c.database ||
		!aliasTargetPattern.MatchString(alias.CollectionName) {
		return "", fmt.Errorf("%w: invalid alias description", ErrResponse)
	}
	return alias.CollectionName, nil
}

func (c *Client) PublishVerified(
	ctx context.Context,
	expectedOld string,
	target *VerifiedTarget,
) (PublicationResult, error) {
	result := PublicationResult{
		State:       PublicationNotAttempted,
		ExpectedOld: expectedOld,
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return result, fmt.Errorf("%w: overall deadline is required", ErrInput)
	}
	if target == nil || target.client != c ||
		target.report.CollectionID <= 0 ||
		!targetPattern.MatchString(target.report.Collection) {
		return result, fmt.Errorf("%w: valid verification token is required", ErrInput)
	}
	result.NewTarget = target.report.Collection

	if expectedOld == result.NewTarget ||
		(expectedOld != "" && !aliasTargetPattern.MatchString(expectedOld)) {
		return result, fmt.Errorf("%w: invalid expected old target", ErrInput)
	}

	// 凭据只能消费一次。失败后不得拿它盲目重发发布。
	if !target.used.CompareAndSwap(false, true) {
		return result, fmt.Errorf("%w: verification token already consumed", ErrInput)
	}

	if err := c.checkReady(ctx, target.report); err != nil {
		return result, err
	}
	old, err := c.GetAliasTarget(ctx)
	if err != nil {
		return result, err
	}
	if old != expectedOld {
		return result, fmt.Errorf("%w: alias changed before publication", ErrRejected)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	path := "aliases/alter"
	if expectedOld == "" {
		path = "aliases/create"
	}
	result.State = PublicationUnknown

	_, writeErr := c.request(ctx, path, map[string]any{
		"aliasName":      c.alias,
		"collectionName": result.NewTarget,
	}, true)
	result.RequestAcknowledged = writeErr == nil
	if writeErr != nil {
		result.RequestError = writeErr.Error()
	}

	// 发布已经尝试后，只做有限只读观察。
	observeCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		10*time.Second,
	)
	defer cancel()

	for attempt := 0; attempt < 3; attempt++ {
		observed, observeErr := c.GetAliasTarget(observeCtx)
		result.ObservationSucceeded = observeErr == nil
		result.ObservationError = ""

		if observeErr != nil {
			result.State = PublicationUnknown
			result.ObservedTarget = ""
			result.ObservationError = observeErr.Error()
		} else {
			result.ObservedTarget = observed
			switch observed {
			case result.NewTarget:
				result.State = PublicationApplied
			case expectedOld:
				result.State = PublicationOldObserved
			default:
				result.State = PublicationUnknown
			}
		}

		if result.State == PublicationApplied ||
			attempt == 2 || observeCtx.Err() != nil {
			break
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-observeCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if observeCtx.Err() != nil {
			break
		}
	}

	// 保留父请求的取消/超时语义，同时返回已取得的发布观察。
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if writeErr != nil {
		return result, writeErr
	}
	if result.State != PublicationApplied {
		return result, fmt.Errorf(
			"%w: publication was not observed at the new target",
			ErrStateUnknown,
		)
	}
	return result, nil
}
