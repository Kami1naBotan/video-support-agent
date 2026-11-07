package evaluation

import "fmt"

const (
	RunCompleted = "completed"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
	RunTimedOut  = "timed_out"
	RunUnknown   = "unknown"

	MaxObservedItems = 1000
)

type RetrievalObservation struct {
	CaseID   string        `json:"case_id"`
	CorpusID string        `json:"corpus_id"`
	TopK     int           `json:"top_k"`
	Status   string        `json:"status"`
	Degraded bool          `json:"degraded"`
	Items    []CitationKey `json:"items"`
}

type RetrievalCaseScore struct {
	CaseID              string   `json:"case_id"`
	CorpusID            string   `json:"corpus_id"`
	TopK                int      `json:"top_k"`
	Status              string   `json:"status"`
	Degraded            bool     `json:"degraded"`
	Applicable          bool     `json:"applicable"`
	RelevantDocuments   int      `json:"relevant_documents"`
	ReturnedItemsAtK    int      `json:"returned_items_at_k"`
	HitDocuments        int      `json:"hit_documents"`
	FirstRelevantRank   int      `json:"first_relevant_rank"`
	Recall              *float64 `json:"recall"`
	ReciprocalRank      *float64 `json:"reciprocal_rank"`
	NegativeWithResults *bool    `json:"negative_with_results"`
}

type RetrievalReport struct {
	TopK                int                  `json:"top_k"`
	TotalCases          int                  `json:"total_cases"`
	ApplicableCases     int                  `json:"applicable_cases"`
	FailedCases         int                  `json:"failed_cases"`
	DegradedCases       int                  `json:"degraded_cases"`
	NegativeCases       int                  `json:"negative_cases"`
	NegativeCompleted   int                  `json:"negative_completed"`
	NegativeWithResults int                  `json:"negative_with_results"`
	RecallAtK           *float64             `json:"recall_at_k"`
	MRRAtK              *float64             `json:"mrr_at_k"`
	Cases               []RetrievalCaseScore `json:"cases"`
}

func ScoreRetrieval(cases []Case, observations []RetrievalObservation) (RetrievalReport, error) {
	if len(cases) > MaxCases || len(observations) > MaxCases {
		return RetrievalReport{}, fieldError("retrieval", "too many records")
	}

	expected := make(map[string]Case, len(cases))
	topK := 0

	for i, c := range cases {
		path := fmt.Sprintf("cases[%d]", i)
		if err := ValidateCase(c); err != nil {
			return RetrievalReport{}, fmt.Errorf("%s: %w", path, err)
		}
		if c.Suite != SuiteRetrieval {
			return RetrievalReport{}, fieldError(path+".suite", "retrieval scorer requires retrieval cases")
		}
		if _, exists := expected[c.ID]; exists {
			return RetrievalReport{}, fieldError(path+".id", "duplicate case ID")
		}
		if i == 0 {
			topK = c.TopK
		} else if c.TopK != topK {
			return RetrievalReport{}, fieldError(path+".top_k", "one report requires a common K")
		}
		expected[c.ID] = c
	}

	actual := make(map[string]RetrievalObservation, len(observations))
	for i, observation := range observations {
		path := fmt.Sprintf("observations[%d]", i)
		c, exists := expected[observation.CaseID]
		if !exists {
			return RetrievalReport{}, fieldError(path+".case_id", "no matching case")
		}
		if _, exists := actual[observation.CaseID]; exists {
			return RetrievalReport{}, fieldError(path+".case_id", "duplicate observation")
		}
		if err := validateRetrievalObservation(c, observation); err != nil {
			return RetrievalReport{}, fmt.Errorf("%s: %w", path, err)
		}
		actual[observation.CaseID] = observation
	}

	for i, c := range cases {
		if _, exists := actual[c.ID]; !exists {
			return RetrievalReport{}, fieldError(fmt.Sprintf("cases[%d]", i), "observation is missing")
		}
	}

	report := RetrievalReport{
		TopK:       topK,
		TotalCases: len(cases),
		Cases:      make([]RetrievalCaseScore, 0, len(cases)),
	}
	var recallSum, reciprocalRankSum float64

	for _, c := range cases {
		observation := actual[c.ID]
		row := scoreRetrievalCase(c, observation)
		report.Cases = append(report.Cases, row)

		if observation.Status != RunCompleted {
			report.FailedCases++
		}
		if observation.Degraded {
			report.DegradedCases++
		}

		if row.Applicable {
			report.ApplicableCases++
			recallSum += *row.Recall
			reciprocalRankSum += *row.ReciprocalRank
			continue
		}

		report.NegativeCases++
		if row.NegativeWithResults != nil {
			report.NegativeCompleted++
			if *row.NegativeWithResults {
				report.NegativeWithResults++
			}
		}
	}

	if report.ApplicableCases > 0 {
		recall := recallSum / float64(report.ApplicableCases)
		mrr := reciprocalRankSum / float64(report.ApplicableCases)
		report.RecallAtK = &recall
		report.MRRAtK = &mrr
	}
	return report, nil
}

func validateRetrievalObservation(c Case, observation RetrievalObservation) error {
	if observation.CorpusID != c.CorpusID {
		return fieldError("corpus_id", "does not match the case")
	}
	if observation.TopK != c.TopK {
		return fieldError("top_k", "does not match the case")
	}
	if !oneOf(observation.Status, RunCompleted, RunFailed, RunCancelled, RunTimedOut, RunUnknown) {
		return fieldError("status", "unsupported run status")
	}
	if observation.Items == nil {
		return fieldError("items", "must be an explicit array")
	}
	if len(observation.Items) > MaxObservedItems {
		return fieldError("items", "too many observed items")
	}

	for i, item := range observation.Items {
		path := fmt.Sprintf("items[%d]", i)
		if err := validateKey(path+".document_key", item.DocumentKey); err != nil {
			return err
		}
		if err := validateKey(path+".version_key", item.VersionKey); err != nil {
			return err
		}
		if err := validateKey(path+".article_key", item.ArticleKey); err != nil {
			return err
		}
	}
	return nil
}

func scoreRetrievalCase(c Case, observation RetrievalObservation) RetrievalCaseScore {
	limit := len(observation.Items)
	if limit > c.TopK {
		limit = c.TopK
	}

	row := RetrievalCaseScore{
		CaseID:            c.ID,
		CorpusID:          c.CorpusID,
		TopK:              c.TopK,
		Status:            observation.Status,
		Degraded:          observation.Degraded,
		Applicable:        len(c.ExpectedDocuments) > 0,
		RelevantDocuments: len(c.ExpectedDocuments),
		ReturnedItemsAtK:  limit,
	}

	if !row.Applicable {
		if observation.Status == RunCompleted {
			hasResults := limit > 0
			row.NegativeWithResults = &hasResults
		}
		return row
	}

	recall := 0.0
	reciprocalRank := 0.0
	row.Recall = &recall
	row.ReciprocalRank = &reciprocalRank

	if observation.Status != RunCompleted {
		return row
	}

	relevant := make(map[string]struct{}, len(c.ExpectedDocuments))
	for _, key := range c.ExpectedDocuments {
		relevant[key] = struct{}{}
	}

	hits := make(map[string]struct{})
	for i := 0; i < limit; i++ {
		documentKey := observation.Items[i].DocumentKey
		if _, ok := relevant[documentKey]; !ok {
			continue
		}

		hits[documentKey] = struct{}{}
		if row.FirstRelevantRank == 0 {
			row.FirstRelevantRank = i + 1
		}
	}

	row.HitDocuments = len(hits)
	recall = float64(row.HitDocuments) / float64(row.RelevantDocuments)
	if row.FirstRelevantRank > 0 {
		reciprocalRank = 1.0 / float64(row.FirstRelevantRank)
	}

	return row
}
