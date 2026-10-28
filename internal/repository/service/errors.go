package service

import (
	"errors"

	"gorm.io/gorm"
)

var (
	ErrInvalidArgument                 = errors.New("invalid argument")
	ErrUserNotFound                    = errors.New("user not found")
	ErrConversationNotFound            = errors.New("conversation not found")
	ErrMessageNotFound                 = errors.New("message not found")
	ErrUsernameExists                  = errors.New("username already exists")
	ErrInvalidCredentials              = errors.New("invalid credentials")
	ErrUnauthorized                    = errors.New("unauthorized")
	ErrConversationBusy                = errors.New("conversation is busy")
	ErrRuleCategoryNotFound            = errors.New("rule category not found")
	ErrKnowledgeDocumentExists         = errors.New("knowledge document already exists")
	ErrKnowledgeDraftNotFound          = errors.New("knowledge draft not found")
	ErrKnowledgeVersionNotFound        = errors.New("knowledge version not found")
	ErrKnowledgeVersionNotEditable     = errors.New("knowledge version is not editable")
	ErrKnowledgeContentConflict        = errors.New("knowledge content revision conflict")
	ErrKnowledgeDocumentNotFound       = errors.New("knowledge document not found")
	ErrKnowledgePublicationConflict    = errors.New("knowledge publication revision conflict")
	ErrKnowledgeVersionNotPublishable  = errors.New("knowledge version is not publishable")
	ErrKnowledgeVerificationRequired   = errors.New("current knowledge content requires verification")
	ErrKnowledgeEffectiveTimeRequired  = errors.New("knowledge effective time is required")
	ErrKnowledgeNotYetEffective        = errors.New("knowledge version is not yet effective")
	ErrKnowledgeExpired                = errors.New("knowledge version has expired")
	ErrKnowledgeCurrentVersionExists   = errors.New("knowledge document already has a current version")
	ErrKnowledgeVersionExists          = errors.New("knowledge version already exists")
	ErrKnowledgeCurrentVersionNotFound = errors.New("no current effective knowledge version")
	ErrKnowledgeArticleNotFound        = errors.New("knowledge article not found")
	ErrKnowledgeVersionNotWithdrawable = errors.New("knowledge version is not withdrawable")
)

func normalizeConversationError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrConversationNotFound
	}

	return err
}
