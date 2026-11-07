package main

import (
	"errors"
	"video-support-agent/internal/observability"

	"video-support-agent/internal/agent"
	"video-support-agent/internal/confirmation"
	"video-support-agent/internal/handler"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

func newAgentMessageHandler(
	db *gorm.DB,
	contexts service.MessageContextStore,
	retriever retrieval.HybridRetriever,
	models llm.AgentModels,
	signer *confirmation.AppealSigner,
	appLogger *zap.Logger,
	observers ...*observability.ExecutionObserver,
) (*handler.MessageHandler, error) {
	if len(observers) > 1 {
		return nil, errors.New("at most one execution observer is allowed")
	}

	var observer *observability.ExecutionObserver
	if len(observers) == 1 {
		observer = observers[0]
	}

	if db == nil || signer == nil {
		return nil, errors.New("initialize Agent dependencies failed")
	}

	conversations := repository.NewConversationRepository(db)
	users := repository.NewUserRepository(db)
	messages := repository.NewMessageRepository(db)
	knowledge := repository.NewKnowledgeRepository(db)

	conversationService := service.NewConversationService(conversations, users)
	knowledgeService := service.NewKnowledgeService(knowledge)

	appealRepository, err := repository.NewAppealTicketRepository(db)
	if err != nil {
		return nil, errors.New("initialize appeal repository failed")
	}

	auditRepository, err := repository.NewAgentAuditRepository(db)
	if err != nil {
		return nil, errors.New("initialize Agent audit repository failed")
	}

	eligibility, err := service.NewAppealEligibilityService(service.NoAppealPolicies{})
	if err != nil {
		return nil, errors.New("initialize appeal eligibility service failed")
	}

	appealService, err := service.NewAppealTicketService(appealRepository, eligibility, signer)
	if err != nil {
		return nil, errors.New("initialize appeal ticket service failed")
	}

	deps := tools.RegistryDependencies{
		Conversations: conversationService,
		Retriever:     retriever,
		Knowledge:     knowledgeService,
		Appeals:       appealService,
		Observation:   observer,
	}

	executor, err := agent.NewExecutor(deps, auditRepository, appealService, models)
	if err != nil {
		return nil, errors.New("initialize Agent executor failed")
	}

	observedExecutor, err := agent.ObserveExecutor(executor, observer)
	if err != nil {
		return nil, errors.New("initialize Agent observation failed")
	}

	messageService, err := service.NewAgentMessageService(messages, conversations, contexts, appLogger, observedExecutor, auditRepository)
	if err != nil {
		return nil, errors.New("initialize Agent message service failed")
	}

	readService, err := service.NewAgentReadService(conversations, messages, auditRepository, observedExecutor)
	if err != nil {
		return nil, errors.New("initialize Agent read service failed")
	}

	messageHandler, err := handler.NewAgentMessageHandler(messageService, readService)
	if err != nil {
		return nil, errors.New("initialize Agent message handler failed")
	}

	return messageHandler, nil
}
