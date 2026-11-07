package main

import (
	"context"
	"errors"
	"time"

	"video-support-agent/internal/config"
	"video-support-agent/internal/elasticsearch"
	"video-support-agent/internal/embedding"
	"video-support-agent/internal/eventbus"
	"video-support-agent/internal/knowledgeworker"
	"video-support-agent/internal/milvus"
	"video-support-agent/internal/outboxrelay"
	"video-support-agent/internal/repository"

	"go.uber.org/zap"
)

func runKnowledgeWorker(ctx context.Context, cfg config.Config, log *zap.Logger, observers ...workerObservation) (runErr error) {
	if len(observers) > 1 {
		return errors.New("worker_observer_config_invalid")
	}

	var observation workerObservation
	if len(observers) == 1 {
		observation = observers[0]
	}
	observer := observation.Async

	settings, err := config.LoadKnowledgeWorkerSettings()
	if err != nil {
		return err
	}
	if _, err := config.ParseKafkaConfig(cfg.Kafka); err != nil {
		return errors.New("kafka_config_invalid")
	}

	definition, err := readWorkerDefinition(settings.ESDefinitionPath)
	if err != nil {
		return err
	}

	var deadline time.Time
	var cleanups []workerCleanup
	defer func() {
		if err := closeWorkerResources(deadline, cleanups); err != nil {
			log.Warn("worker cleanup not confirmed",
				zap.String("code", err.Error()))
			if runErr == nil {
				runErr = err
			}
		}
	}()

	startupCtx, startupCancel := context.WithTimeout(ctx, 30*time.Second)
	defer startupCancel()

	db, pool, err := openWorkerDatabase(startupCtx, cfg.Database)
	if err != nil {
		return errors.New("worker_database_start_failed")
	}
	cleanups = append(cleanups, func(context.Context) error {
		return pool.Close()
	})

	if err := checkKnowledgeWorkerSchema(startupCtx, db); err != nil {
		return err
	}
	if err := checkWorkerSource(startupCtx, db, cfg, settings); err != nil {
		return err
	}

	producer, err := eventbus.NewClient(cfg.Kafka)
	if err != nil {
		return errors.New("kafka_client_create_failed")
	}
	cleanups = append(cleanups, producer.Close)

	es, err := elasticsearch.NewClient(cfg.Elasticsearch)
	if err != nil {
		return errors.New("worker_es_config_invalid")
	}

	cleanups = append(cleanups, func(context.Context) error {
		es.Close()
		return nil
	})

	vector, err := milvus.NewClient(cfg.Milvus)
	if err != nil {
		return errors.New("worker_milvus_config_invalid")
	}

	cleanups = append(cleanups, func(context.Context) error {
		vector.Close()
		return nil
	})

	embedder, err := embedding.NewClient(cfg.Embedding)
	if err != nil {
		return errors.New("worker_embedding_config_invalid")
	}
	cleanups = append(cleanups, func(context.Context) error {
		embedder.Close()
		return nil
	})

	outboxStore, err := repository.NewOutboxRepository(db)
	if err != nil {
		return errors.New("outbox_repository_create_failed")
	}

	publisher := eventbus.ObservePublisher(producer, observer)
	relay, err := outboxrelay.New(outboxStore, publisher, log)
	if err != nil {
		return errors.New("outbox_relay_create_failed")
	}

	buildStore, err := repository.NewKnowledgeBuildRepository(db, settings.GenerationLimit)
	if err != nil {
		return errors.New("build_repository_create_failed")
	}

	builder, err := knowledgeworker.NewDualBuilder(
		buildStore, embedder, es, vector,
		knowledgeworker.DualBuildOptions{
			ExpectedDatabase:   cfg.Database.Name,
			ExpectedServerUUID: settings.ExpectedServerUUID,
			ExpectedDigest:     cfg.Embedding.ModelDigest,
			ESDefinition:       definition,
		},
	)
	if err != nil {
		return errors.New("dual_builder_create_failed")
	}

	runtime, err := knowledgeworker.NewRuntime(buildStore, builder, log, observer)
	if err != nil {
		return errors.New("worker_runtime_create_failed")
	}

	inboxStore, err := repository.NewKnowledgeInboxRepository(db)
	if err != nil {
		return errors.New("inbox_repository_create_failed")
	}

	sourceHandler, err := knowledgeworker.NewSourceHandler(inboxStore)
	if err != nil {
		return errors.New("source_handler_create_failed")
	}

	sourceHandler, err = knowledgeworker.NewObservedHandler(sourceHandler, log)
	if err != nil {
		return errors.New("source_observer_create_failed")
	}

	sourceHandler, err = knowledgeworker.NewIsolatingHandler(sourceHandler, buildStore)
	if err != nil {
		return errors.New("source_isolator_create_failed")
	}

	sourceHandler = eventbus.ObserveHandler(sourceHandler, observer)

	sourceConsumer, err := eventbus.NewConsumer(cfg.Kafka, settings.SourceGroup, eventbus.TopicDocumentEvents)
	if err != nil {
		return errors.New("source_consumer_create_failed")
	}

	retryHandler, err := knowledgeworker.NewRetryHandler(buildStore)
	if err != nil {
		return errors.New("retry_handler_create_failed")
	}

	retryHandler, err = knowledgeworker.NewObservedHandler(retryHandler, log)
	if err != nil {
		return errors.New("retry_observer_create_failed")
	}

	retryHandler, err = knowledgeworker.NewIsolatingHandler(retryHandler, buildStore)
	if err != nil {
		return errors.New("retry_isolator_create_failed")
	}

	retryHandler = eventbus.ObserveHandler(retryHandler, observer)

	retryConsumer, err := eventbus.NewConsumer(cfg.Kafka, settings.RetryGroup, eventbus.TopicIndexRetry)
	if err != nil {
		return errors.New("retry_consumer_create_failed")
	}

	if err := producer.Ping(startupCtx); err != nil {
		return errors.New("kafka_ping_failed")
	}
	if err := producer.CheckTopics(startupCtx); err != nil {
		return errors.New("kafka_topic_check_failed")
	}
	if _, err := embedder.InspectIdentity(startupCtx); err != nil {
		return errors.New("worker_embedding_identity_failed")
	}

	if startupCtx.Err() != nil {
		return errors.New("worker_startup_interrupted")
	}
	startupCancel()

	lagTargets := []knowledgeworker.LagTarget{
		{
			Group: settings.SourceGroup,
			Topic: eventbus.TopicDocumentEvents,
		},
		{
			Group: settings.RetryGroup,
			Topic: eventbus.TopicIndexRetry,
		},
	}

	var lagMetrics *knowledgeworker.LagMetrics
	if observation.Runtime != nil {
		lagMetrics, err = knowledgeworker.NewLagMetrics(observation.Runtime, lagTargets)
		if err != nil {
			return errors.New("worker_lag_metrics_create_failed")
		}
	}

	log.Info("worker started",
		zap.String("source_group", settings.SourceGroup),
		zap.String("retry_group", settings.RetryGroup),
		zap.Uint64("generation_limit", settings.GenerationLimit),
		zap.Int("runtime_units", 7))

	deadline, runErr = runWorkerUnitsWithProbes(ctx, []workerUnit{
		{
			name: "outbox_relay",
			run:  relay.Run,
		},
		{
			name: "source_consumer",
			run: func(ctx context.Context) error {
				return sourceConsumer.Run(ctx, sourceHandler)
			},
		},
		{
			name: "retry_consumer",
			run: func(ctx context.Context) error {
				return retryConsumer.Run(ctx, retryHandler)
			},
		},
		{
			name: "generation_builder",
			run: func(ctx context.Context) error {
				return retryKnowledgeLoop(ctx, "generation_builder", runtime.RunBuilds, log)
			},
		},
		{
			name: "event_settlement",
			run: func(ctx context.Context) error {
				return retryKnowledgeLoop(ctx, "event_settlement", runtime.RunSettlements, log)
			},
		},
		{
			name: "build_recovery",
			run: func(ctx context.Context) error {
				return retryKnowledgeLoop(ctx, "build_recovery", runtime.RunRecoveries, log)
			},
		},
		{
			name: "knowledge_observation",
			run: func(ctx context.Context) error {
				return knowledgeworker.RunObservations(ctx, buildStore, producer, log, lagTargets, lagMetrics)
			},
		},
	}, observation.Probes)
	return runErr
}

func retryKnowledgeLoop(ctx context.Context, name string, run func(context.Context) error, log *zap.Logger) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := run(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !errors.Is(err, knowledgeworker.ErrBuildStore) {
			return err
		}

		log.Warn("knowledge loop waiting for persistence",
			zap.String("unit", name),
			zap.String("code", "persistence_unconfirmed"))

		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
