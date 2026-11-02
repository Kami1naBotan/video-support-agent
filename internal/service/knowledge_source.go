package service

import (
	"fmt"
	"video-support-agent/internal/rulesource"
)

func ValidateKnowledgeOfficialSourceURL(raw string) error {
	if err := rulesource.ValidateOfficialURL(raw); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}
	return nil
}
