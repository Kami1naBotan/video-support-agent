package elasticsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	indexing "video-support-agent/internal/index"
)

const aliasRecoveryTimeout = 5 * time.Second

type aliasRemoveAction struct {
	Index     string `json:"index"`
	Alias     string `json:"alias"`
	MustExist bool   `json:"must_exist"`
}

type aliasAddAction struct {
	Index        string `json:"index"`
	Alias        string `json:"alias"`
	IsWriteIndex bool   `json:"is_write_index"`
}

type aliasAction struct {
	Remove *aliasRemoveAction `json:"remove,omitempty"`
	Add    *aliasAddAction    `json:"add,omitempty"`
}

func (c *Client) SwitchAlias(ctx context.Context, expectedOld string, newIndex string) (indexing.PublicationResult, error) {
	result := indexing.PublicationResult{
		State: indexing.PublicationNotAttempted,
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !indexNamePattern.MatchString(newIndex) || newIndex == c.alias || newIndex == expectedOld {
		return result, fmt.Errorf("%w: invalid new alias target", ErrRejected)
	}
	if expectedOld != "" && (!indexNamePattern.MatchString(expectedOld) || expectedOld == c.alias) {
		return result, fmt.Errorf("%w: invalid expected alias target", ErrRejected)
	}

	current, err := c.GetAliasTarget(ctx)
	if err != nil {
		return result, fmt.Errorf("preflight alias read: %w", err)
	}
	result.ObservedTarget = current

	if current != expectedOld {
		return result, fmt.Errorf("%w: alias target changed since rebuild started", ErrRejected)
	}
	if err := c.requireUnaliasedIndex(ctx, newIndex); err != nil {
		return result, err
	}

	actions := make([]aliasAction, 0, 2)
	if expectedOld != "" {
		actions = append(actions, aliasAction{
			Remove: &aliasRemoveAction{
				Index:     expectedOld,
				Alias:     c.alias,
				MustExist: true,
			},
		})
	}
	actions = append(actions, aliasAction{
		Add: &aliasAddAction{
			Index:        newIndex,
			Alias:        c.alias,
			IsWriteIndex: false,
		},
	})

	payload := struct {
		Actions []aliasAction `json:"actions"`
	}{
		Actions: actions,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return result, fmt.Errorf("%w: cannot encode alias actions", ErrRejected)
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}

	result.State = indexing.PublicationUnknown
	result.ObservedTarget = ""

	status, response, requestErr := c.request(ctx, http.MethodPost, "/_aliases", "application/json", body)

	var writeErr error
	switch {
	case requestErr != nil:
		writeErr = fmt.Errorf("%w: alias request: %w", ErrStateUnknown, requestErr)

	case status != http.StatusOK:
		writeErr = fmt.Errorf("%w: alias request returned HTTP %d", ErrStateUnknown, status)

	default:
		var acknowledgement struct {
			Acknowledged *bool `json:"acknowledged"`
			Errors       bool  `json:"errors"`
		}
		if json.Unmarshal(response, &acknowledgement) != nil || acknowledgement.Acknowledged == nil || !*acknowledgement.Acknowledged || acknowledgement.Errors {
			writeErr = fmt.Errorf("%w: %w: alias acknowledgement not confirmed", ErrStateUnknown, ErrResponse)
		} else {
			result.RequestAcknowledged = true
		}
	}

	if contextErr := ctx.Err(); contextErr != nil {
		writeErr = errors.Join(writeErr, contextErr)
	}

	observeCtx := ctx
	cancel := func() {}
	if writeErr != nil {
		observeCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), aliasRecoveryTimeout)
	}
	defer cancel()

	observed, readErr := c.GetAliasTarget(observeCtx)
	if readErr != nil {
		return result, errors.Join(fmt.Errorf("%w: cannot observe alias after publication request", ErrStateUnknown), writeErr, readErr)
	}

	result.ObservedTarget = observed
	if observed == newIndex {
		result.State = indexing.PublicationApplied

		return result, writeErr
	}

	if observed == expectedOld {
		result.State = indexing.PublicationOldObserved
	}

	return result, errors.Join(fmt.Errorf("%w: observation does not confirm the new alias target", ErrStateUnknown), writeErr)
}
