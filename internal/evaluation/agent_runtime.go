package evaluation

import (
	"context"
	"fmt"
	"regexp"

	"video-support-agent/internal/agent"
	"video-support-agent/internal/confirmation"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"
	"video-support-agent/internal/service"
	"video-support-agent/internal/tools"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var evaluationDatabaseName = regexp.MustCompile(`^eval_[a-z0-9_]{1,48}$`)

type AgentRunner struct {
	db       *gorm.DB
	database string
	binding  *CorpusBinding
	executor *agent.Executor
	appeals  *service.AppealTicketService
}

func NewAgentRunner(ctx context.Context, db *gorm.DB, database string, binding *CorpusBinding, retriever retrieval.HybridRetriever, models llm.AgentModels, signer *confirmation.AppealSigner) (*AgentRunner, error) {
	if ctx == nil || db == nil ||
		binding == nil || binding.corpus == nil ||
		!binding.corpus.Synthetic() ||
		isNilRetriever(retriever) || signer == nil ||
		!evaluationDatabaseName.MatchString(database) {
		return nil, fieldError("agent_runner", "invalid isolated configuration")
	}
	if db.Dialector.Name() != "mysql" {
		return nil, fieldError("agent_runner", "requires isolated MySQL")
	}

	db = db.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	runner := &AgentRunner{
		db: db, database: database, binding: binding,
	}
	if err := runner.checkDatabase(ctx); err != nil {
		return nil, err
	}

	for key, article := range binding.corpus.articles {
		versionKey := corpusVersionKey(key.DocumentKey, key.VersionKey)
		bound := ArticleBinding{
			Key: key,
			IDs: RuntimeArticleID{
				DocumentID: binding.forward[0][key.DocumentKey],
				VersionID:  binding.forward[1][versionKey],
				ArticleID:  binding.forward[2][versionKey+"/"+key.ArticleKey],
			},
			CategoryID: binding.forward[3][article.CategoryKey],
		}
		if err := verifyDatabaseArticle(ctx, db, bound, article); err != nil {
			return nil, fieldError("agent_runner", "database corpus mismatch")
		}
	}

	appeals, err := repository.NewAppealTicketRepository(db)
	if err != nil {
		return nil, fieldError("agent_runner", "cannot initialize appeal store")
	}
	audit, err := repository.NewAgentAuditRepository(db)
	if err != nil {
		return nil, fieldError("agent_runner", "cannot initialize audit store")
	}
	eligibility, err := service.NewAppealEligibilityService(service.NoAppealPolicies{})
	if err != nil {
		return nil, fieldError("agent_runner", "cannot initialize eligibility")
	}
	runner.appeals, err = service.NewAppealTicketService(appeals, eligibility, signer)
	if err != nil {
		return nil, fieldError("agent_runner", "cannot initialize appeal service")
	}

	conversations := service.NewConversationService(repository.NewConversationRepository(db), repository.NewUserRepository(db))
	deps := tools.RegistryDependencies{
		Conversations: conversations,
		Retriever:     corpusAgentRetriever{inner: retriever, binding: binding},
		Knowledge:     service.NewKnowledgeService(repository.NewKnowledgeRepository(db)),
		Appeals:       runner.appeals,
	}
	runner.executor, err = agent.NewExecutor(
		deps, audit, runner.appeals, models,
	)
	if err != nil {
		return nil, fieldError("agent_runner", "cannot initialize executor")
	}
	return runner, nil
}

func (r *AgentRunner) checkDatabase(ctx context.Context) error {
	var actual string
	if err := r.db.WithContext(ctx).Raw("SELECT DATABASE()").Scan(&actual).Error; err != nil || actual != r.database {
		return fieldError("agent_runner", "isolated database check failed")
	}
	return nil
}

func (r *AgentRunner) newOwner(ctx context.Context, runID, caseID string) (service.AgentAuditOwner, error) {
	var owner service.AgentAuditOwner
	if err := r.checkDatabase(ctx); err != nil {
		return owner, err
	}

	digest, err := EvidenceHash([]string{runID, caseID})
	if err != nil {
		return owner, err
	}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		user := model.User{
			Username:     "eval_" + digest[:40],
			PasswordHash: "disabled-evaluation-account",
			Role:         model.RoleUser,
		}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}

		conversation := model.Conversation{
			UserID: user.ID,
			Title:  "evaluation:" + caseID,
		}
		if err := tx.Omit("User").Create(&conversation).Error; err != nil {
			return err
		}
		owner = service.AgentAuditOwner{
			UserID: user.ID, ConversationID: conversation.ID,
		}
		return nil
	})
	if err != nil {
		return service.AgentAuditOwner{}, fieldError("agent_runner", "cannot create isolated owner")
	}
	return owner, nil
}

type corpusAgentRetriever struct {
	inner   retrieval.HybridRetriever
	binding *CorpusBinding
}

func (r corpusAgentRetriever) Retrieve(ctx context.Context, query string, options retrieval.HybridOptions) (retrieval.HybridResult, error) {
	result, err := r.inner.Retrieve(ctx, query, options)
	if err != nil {
		return result, err
	}
	if len(result.Items) > MaxObservedItems || !validHybridResult(result) {
		return retrieval.HybridResult{}, fmt.Errorf("invalid evaluation retrieval")
	}
	for _, item := range result.Items {
		if _, err := r.binding.MatchItem(item); err != nil {
			return retrieval.HybridResult{}, fmt.Errorf("evaluation corpus mismatch")
		}
	}
	return result, nil
}
