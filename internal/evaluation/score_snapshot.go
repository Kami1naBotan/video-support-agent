package evaluation

import "fmt"

type EvaluationIdentity struct {
	SourceSHA256      string `json:"source_sha256"`
	InputsSHA256      string `json:"inputs_sha256"`
	CorpusSHA256      string `json:"corpus_sha256"`
	BindingSHA256     string `json:"binding_sha256"`
	ModelConfigSHA256 string `json:"model_config_sha256"`
	PlannerModel      string `json:"planner_model"`
	AnswerModel       string `json:"answer_model"`
	EmbeddingModel    string `json:"embedding_model"`
}

type ScoreSnapshot struct {
	SchemaVersion      int                    `json:"schema_version"`
	RunID              string                 `json:"run_id"`
	Mode               string                 `json:"mode"`
	RunIndex           int                    `json:"run_index"`
	PlannedCases       int                    `json:"planned_cases"`
	Identity           EvaluationIdentity     `json:"identity"`
	CasesSHA256        string                 `json:"cases_sha256"`
	ObservationsSHA256 string                 `json:"observations_sha256"`
	Cases              []Case                 `json:"cases"`
	Retrieval          []RetrievalObservation `json:"retrieval"`
	Agents             []AgentObservation     `json:"agents"`
}

type ReviewDocument struct {
	SchemaVersion      int           `json:"schema_version"`
	SnapshotFileSHA256 string        `json:"snapshot_file_sha256"`
	Reviews            []AgentReview `json:"reviews"`
}

type FrozenReport struct {
	SchemaVersion      int              `json:"schema_version"`
	RunID              string           `json:"run_id"`
	Mode               string           `json:"mode"`
	RunIndex           int              `json:"run_index"`
	SnapshotFileSHA256 string           `json:"snapshot_file_sha256"`
	ReviewFileSHA256   string           `json:"review_file_sha256"`
	ScorerBinarySHA256 string           `json:"scorer_binary_sha256"`
	Retrieval          *RetrievalReport `json:"retrieval"`
	Agent              *AgentReport     `json:"agent"`
}

func snapshotObservationDigest(snapshot ScoreSnapshot) (string, error) {
	return EvidenceHash(struct {
		Retrieval []RetrievalObservation `json:"retrieval"`
		Agents    []AgentObservation     `json:"agents"`
	}{
		Retrieval: snapshot.Retrieval,
		Agents:    snapshot.Agents,
	})
}

func FreezeScoreSnapshot(snapshot ScoreSnapshot) (ScoreSnapshot, error) {
	var err error
	snapshot.CasesSHA256, err = EvidenceHash(snapshot.Cases)
	if err != nil {
		return ScoreSnapshot{}, err
	}
	snapshot.ObservationsSHA256, err = snapshotObservationDigest(snapshot)
	if err != nil {
		return ScoreSnapshot{}, err
	}
	if _, err := ScoreSnapshotData(snapshot, nil); err != nil {
		return ScoreSnapshot{}, err
	}
	return snapshot, nil
}

func ScoreSnapshotData(snapshot ScoreSnapshot, reviews []AgentReview) (FrozenReport, error) {
	report := FrozenReport{
		SchemaVersion: SchemaVersion,
		RunID:         snapshot.RunID,
		Mode:          snapshot.Mode,
		RunIndex:      snapshot.RunIndex,
	}

	if snapshot.SchemaVersion != SchemaVersion ||
		snapshot.RunIndex < 1 ||
		snapshot.PlannedCases < 1 ||
		snapshot.PlannedCases > MaxCases ||
		len(snapshot.Cases) != snapshot.PlannedCases {
		return report, fieldError("snapshot", "invalid version, run index or count")
	}
	if err := validateKey("run_id", snapshot.RunID); err != nil {
		return report, err
	}
	if !oneOf(snapshot.Mode, "retrieval", "chat", "confirm", "ticket_query") {
		return report, fieldError("snapshot.mode", "unsupported mode")
	}
	if snapshot.Cases == nil || snapshot.Retrieval == nil || snapshot.Agents == nil {
		return report, fieldError("snapshot", "arrays must be explicit")
	}

	identity := snapshot.Identity
	for _, digest := range []string{
		identity.SourceSHA256,
		identity.InputsSHA256,
		identity.CorpusSHA256,
		identity.BindingSHA256,
		identity.ModelConfigSHA256,
	} {
		if !validDigest(digest) {
			return report, fieldError("snapshot.identity", "invalid digest")
		}
	}
	for _, name := range []string{identity.PlannerModel, identity.AnswerModel, identity.EmbeddingModel} {
		if err := validateText("snapshot.model", name, 256); err != nil {
			return report, err
		}
	}

	caseDigest, err := EvidenceHash(snapshot.Cases)
	if err != nil || caseDigest != snapshot.CasesSHA256 {
		return report, fieldError("snapshot", "case digest mismatch")
	}
	observationDigest, err := snapshotObservationDigest(snapshot)
	if err != nil || observationDigest != snapshot.ObservationsSHA256 {
		return report, fieldError("snapshot", "observation digest mismatch")
	}

	if snapshot.Mode == "retrieval" {
		if len(snapshot.Agents) != 0 || len(reviews) != 0 {
			return report, fieldError("snapshot", "retrieval cannot include Agent data")
		}
		result, err := ScoreRetrieval(snapshot.Cases, snapshot.Retrieval)
		if err != nil {
			return report, err
		}
		report.Retrieval = &result
		return report, nil
	}

	if len(snapshot.Retrieval) != 0 {
		return report, fieldError("snapshot", "Agent mode cannot include retrieval rows")
	}
	for i, observation := range snapshot.Agents {
		if observation.RunID != snapshot.RunID || observation.CorpusSHA256 != identity.CorpusSHA256 {
			return report, fieldError(fmt.Sprintf("agents[%d]", i), "run or corpus digest mismatch")
		}
	}
	result, err := ScoreAgents(snapshot.Cases, snapshot.Agents, reviews)
	if err != nil {
		return report, err
	}
	report.Agent = &result
	return report, nil
}

func NewReviewDraft(snapshot ScoreSnapshot, snapshotFileSHA256 string) (ReviewDocument, error) {
	document := ReviewDocument{
		SchemaVersion:      SchemaVersion,
		SnapshotFileSHA256: snapshotFileSHA256,
		Reviews:            make([]AgentReview, 0, len(snapshot.Cases)),
	}
	if !validDigest(snapshotFileSHA256) {
		return document, fieldError("review", "invalid snapshot file digest")
	}
	if snapshot.Mode == "retrieval" {
		return document, fieldError("review", "requires an Agent snapshot")
	}
	if _, err := ScoreSnapshotData(snapshot, nil); err != nil {
		return document, err
	}

	observations := make(map[string]AgentObservation, len(snapshot.Agents))
	for _, observation := range snapshot.Agents {
		observations[observation.CaseID] = observation
	}

	for _, c := range snapshot.Cases {
		observation := observations[c.ID]
		caseDigest, err := EvidenceHash(c)
		if err != nil {
			return document, err
		}
		observationDigest, err := EvidenceHash(observation)
		if err != nil {
			return document, err
		}

		review := AgentReview{
			CaseID:                c.ID,
			CaseSHA256:            caseDigest,
			ObservationSHA256:     observationDigest,
			Citations:             make([]CitationReview, 0),
			RequiredPointsMet:     make([]*bool, len(c.RequiredPoints)),
			ForbiddenClaimsAbsent: make([]*bool, len(c.ForbiddenClaims)),
			Evidence:              "待人工复核，尚未判定。",
		}
		seen := make(map[CitationKey]struct{})
		for _, key := range observation.Citations {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			review.Citations = append(review.Citations, CitationReview{
				Key:      key,
				Evidence: "待核对条款身份、适用范围及全部关联主张。",
			})
		}
		document.Reviews = append(document.Reviews, review)
	}
	return document, nil
}
