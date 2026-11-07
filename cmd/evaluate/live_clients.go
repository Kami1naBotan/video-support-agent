package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/confirmation"
	"video-support-agent/internal/database"
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/evaluation"
	"video-support-agent/internal/generationquery"
	"video-support-agent/internal/llm"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/retrieval"

	"gorm.io/gorm"
)

type liveClients struct {
	Binding   *evaluation.CorpusBinding
	Retriever retrieval.HybridRetriever
	Runner    *evaluation.AgentRunner
	Target    generationquery.Target
	Close     func() error
	Check     func(context.Context) error
}

type fixedGeneration struct {
	inner  generationquery.Resolver
	target generationquery.Target
}

func (f fixedGeneration) ResolveActive(ctx context.Context) (generationquery.Target, error) {
	current, err := f.inner.ResolveActive(ctx)
	if err != nil || current != f.target {
		return generationquery.Target{}, errors.New("evaluation generation changed")
	}
	return current, nil
}

func verifyDatabaseTarget(ctx context.Context, db *gorm.DB, c liveConfig) error {
	var actual struct {
		DatabaseName string
		ServerUUID   string
	}
	if err := db.WithContext(ctx).
		Raw("SELECT DATABASE() AS database_name, @@server_uuid AS server_uuid").
		Scan(&actual).Error; err != nil ||
		actual.DatabaseName != c.Database.Name ||
		actual.ServerUUID != c.Database.ServerUUID {
		return errors.New("database target does not match evaluation configuration")
	}

	var generation struct {
		SourceDatabase   string
		SourceServerUUID string
	}
	if err := db.WithContext(ctx).
		Table("knowledge_index_generations").
		Select("source_database, source_server_uuid").
		Where("id = ?", c.GenerationID).
		Take(&generation).Error; err != nil ||
		generation.SourceDatabase != actual.DatabaseName ||
		generation.SourceServerUUID != actual.ServerUUID {
		return errors.New("generation source does not match evaluation database")
	}
	return nil
}

func openLiveClients(ctx context.Context, input liveInputs, corpus *evaluation.Corpus) (result *liveClients, returnErr error) {
	c := input.Config
	password := os.Getenv("EVAL_DB_PASSWORD")
	if password == "" {
		return nil, errors.New("EVAL_DB_PASSWORD is required")
	}

	db, err := database.OpenMySQL(config.DatabaseConfig{
		Host: c.Database.Host, Port: c.Database.Port,
		User: c.Database.User, Password: password, Name: c.Database.Name,
	})
	if err != nil {
		return nil, errors.New("cannot open evaluation database")
	}
	pool, err := db.DB()
	if err != nil {
		return nil, errors.New("cannot obtain evaluation connection pool")
	}

	var closeClients []func()
	closeAll := func() error {
		for i := len(closeClients) - 1; i >= 0; i-- {
			closeClients[i]()
		}
		return pool.Close()
	}
	defer func() {
		if returnErr != nil {
			_ = closeAll()
		}
	}()

	if err := verifyDatabaseTarget(ctx, db, c); err != nil {
		return nil, err
	}
	var articleCount int64
	if err := db.WithContext(ctx).Table("rule_articles").Count(&articleCount).Error; err != nil || articleCount != int64(len(input.Bindings)) {
		return nil, errors.New("isolated database must contain exactly the bound articles")
	}

	binding, err := evaluation.BindCorpus(ctx, db, corpus, input.Bindings)
	if err != nil {
		return nil, errors.New("cannot bind evaluation corpus")
	}
	active, err := repository.NewKnowledgeActiveRepository(db)
	if err != nil {
		return nil, errors.New("cannot initialize generation resolver")
	}
	target, err := active.ResolveActive(ctx)
	if err != nil || target.GenerationID != c.GenerationID {
		return nil, errors.New("expected generation is not active")
	}

	search, err := elasticsearch.NewClient(c.Elasticsearch)
	if err != nil {
		return nil, errors.New("cannot initialize Elasticsearch")
	}
	closeClients = append(closeClients, search.Close)

	embed, err := embedding.NewClient(c.Embedding)
	if err != nil {
		return nil, errors.New("cannot initialize embedding")
	}
	closeClients = append(closeClients, embed.Close)

	identity, err := embed.InspectIdentity(ctx)
	if err != nil || identity != target.Identity {
		return nil, errors.New("embedding identity does not match published generation")
	}

	vector, err := milvus.NewClient(c.Milvus)
	if err != nil {
		return nil, errors.New("cannot initialize Milvus")
	}
	closeClients = append(closeClients, vector.Close)

	const vectorTimeout = 35 * time.Second
	searcher, err := milvus.NewSearcher(embed, vector, vectorTimeout)
	if err != nil {
		return nil, errors.New("cannot initialize vector searcher")
	}
	knowledge := repository.NewKnowledgeRepository(db)
	fixed := fixedGeneration{inner: active, target: target}
	retrievers, err := generationquery.NewRetrievers(fixed, search, searcher, knowledge, knowledge, nil, vectorTimeout)
	if err != nil {
		return nil, errors.New("cannot initialize generation retrieval")
	}

	result = &liveClients{
		Binding: binding, Retriever: retrievers.Hybrid,
		Target: target, Close: closeAll,
	}
	result.Check = func(checkCtx context.Context) error {
		if err := verifyDatabaseTarget(checkCtx, db, c); err != nil {
			return err
		}
		if _, err := fixed.ResolveActive(checkCtx); err != nil {
			return err
		}
		after, err := embed.InspectIdentity(checkCtx)
		if err != nil || after != identity {
			return errors.New("embedding identity changed")
		}
		afterBinding, err := evaluation.BindCorpus(checkCtx, db, corpus, input.Bindings)
		if err != nil || afterBinding.Digest() != binding.Digest() {
			return errors.New("corpus binding changed")
		}
		return nil
	}

	if c.Mode != "retrieval" {
		apiKey := os.Getenv("EVAL_LLM_API_KEY")
		if apiKey == "" {
			return nil, errors.New("EVAL_LLM_API_KEY is required")
		}
		models, err := llm.NewAgentModels(ctx, config.LLMConfig{
			BaseURL: c.LLMEndpoint,
			Model:   c.LLMModel,
			APIKey:  apiKey,
			Timeout: c.LLMTimeout,
		})
		if err != nil {
			return nil, errors.New("cannot initialize Agent models")
		}

		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, errors.New("cannot generate evaluation confirmation key")
		}
		signer, err := confirmation.NewAppealSigner(
			config.AppealConfirmationConfig{
				KeyBase64: base64.StdEncoding.EncodeToString(key),
				Issuer:    "isolated-evaluation",
				Audience:  "isolated-appeal-create",
			},
		)
		clear(key)
		if err != nil {
			return nil, errors.New("cannot initialize confirmation signer")
		}

		result.Runner, err = evaluation.NewAgentRunner(ctx, db, c.Database.Name, binding, retrievers.Hybrid, models, signer)
		if err != nil {
			return nil, errors.New("cannot initialize isolated Agent runner")
		}
	}
	return result, nil
}
