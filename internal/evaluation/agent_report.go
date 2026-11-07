package evaluation

import "fmt"

func ScoreAgents(cases []Case, observations []AgentObservation, reviews []AgentReview) (AgentReport, error) {
	if len(cases) > MaxCases || len(observations) > MaxCases || len(reviews) > MaxCases {
		return AgentReport{}, fieldError("agent", "too many records")
	}

	caseIDs := make(map[string]struct{}, len(cases))
	for i, c := range cases {
		if _, exists := caseIDs[c.ID]; exists {
			return AgentReport{}, fieldError(fmt.Sprintf("cases[%d]", i), "duplicate case ID")
		}
		caseIDs[c.ID] = struct{}{}
	}

	actual := make(map[string]AgentObservation, len(observations))
	runID := ""
	corpusDigests := make(map[string]string)
	for i, o := range observations {
		path := fmt.Sprintf("observations[%d]", i)
		if _, exists := caseIDs[o.CaseID]; !exists {
			return AgentReport{}, fieldError(path, "no matching case")
		}
		if _, exists := actual[o.CaseID]; exists {
			return AgentReport{}, fieldError(path, "duplicate observation")
		}
		if i == 0 {
			runID = o.RunID
		} else if o.RunID != runID {
			return AgentReport{}, fieldError(path, "mixed run IDs")
		}
		if digest, exists := corpusDigests[o.CorpusID]; exists &&
			digest != o.CorpusSHA256 {
			return AgentReport{}, fieldError(path, "conflicting corpus digests")
		}
		corpusDigests[o.CorpusID] = o.CorpusSHA256
		actual[o.CaseID] = o
	}

	reviewByCase := make(map[string]AgentReview, len(reviews))
	for i, review := range reviews {
		path := fmt.Sprintf("reviews[%d]", i)
		if _, exists := caseIDs[review.CaseID]; !exists {
			return AgentReport{}, fieldError(path, "no matching case")
		}
		if _, exists := reviewByCase[review.CaseID]; exists {
			return AgentReport{}, fieldError(path, "duplicate review")
		}
		reviewByCase[review.CaseID] = review
	}

	report := AgentReport{
		RunID:      runID,
		TotalCases: len(cases),
		Cases:      make([]AgentCaseScore, 0, len(cases)),
	}
	for i, c := range cases {
		o, exists := actual[c.ID]
		if !exists {
			return AgentReport{}, fieldError(
				fmt.Sprintf("cases[%d]", i), "observation is missing",
			)
		}
		var review *AgentReview
		if saved, exists := reviewByCase[c.ID]; exists {
			review = &saved
		}
		row, err := ScoreAgentCase(c, o, review)
		if err != nil {
			return AgentReport{}, fmt.Errorf("cases[%d]: %w", i, err)
		}
		report.Cases = append(report.Cases, row)
		accumulateAgent(&report, row)
	}

	report.HandoffAccuracy = makeMetric(
		report.Handoff.TP+report.Handoff.TN, report.TotalCases, 0,
	)
	report.HandoffPrecision = makeMetric(
		report.Handoff.TP, report.Handoff.TP+report.Handoff.FP, 0,
	)
	report.HandoffRecall = makeMetric(
		report.Handoff.TP, report.Handoff.Positive, 0,
	)

	for _, metric := range []*Metric{
		&report.ToolAccuracy,
		&report.WithToolsAccuracy,
		&report.WithoutToolsAccuracy,
		&report.ToolSelectionAccuracy,
		&report.DecisionAccuracy,
		&report.CitationIdentityPrecision,
		&report.CitationSemanticAccuracy,
		&report.ExpectedCitationCoverage,
	} {
		*metric = makeMetric(metric.Correct, metric.Total, metric.Pending)
	}
	return report, nil
}

func accumulateAgent(report *AgentReport, row AgentCaseScore) {
	addTrial(&report.ToolAccuracy, row.ToolMatch)
	addTrial(&report.ToolSelectionAccuracy, row.ToolSelectionMatch)
	addTrial(&report.DecisionAccuracy, row.DecisionMatch)
	if row.ExpectedTools {
		addTrial(&report.WithToolsAccuracy, row.ToolMatch)
	} else {
		addTrial(&report.WithoutToolsAccuracy, row.ToolMatch)
	}

	handoff := &report.Handoff
	if row.ExpectedHandoff {
		handoff.Positive++
	} else {
		handoff.Negative++
	}

	if row.Failed {
		report.FailedCases++
		if row.ExpectedHandoff {
			handoff.FailedPositive++
		} else {
			handoff.FailedNegative++
		}
	} else {
		switch {
		case row.ExpectedHandoff && row.PredictedHandoff:
			handoff.TP++
		case row.ExpectedHandoff && !row.PredictedHandoff:
			handoff.FN++
		case !row.ExpectedHandoff && row.PredictedHandoff:
			handoff.FP++
		default:
			handoff.TN++
		}
	}

	report.CitationIdentityPrecision.Correct += row.IdentityMatches
	report.CitationIdentityPrecision.Total += row.CitationCount
	report.CitationSemanticAccuracy.Correct += row.SemanticCorrect
	report.CitationSemanticAccuracy.Total += row.CitationCount
	report.CitationSemanticAccuracy.Pending += row.PendingCitations

	report.ExpectedCitationCoverage.Correct += row.ExpectedCitationCount - row.MissingCitations
	report.ExpectedCitationCoverage.Total += row.ExpectedCitationCount
	if row.MissingCitations > 0 {
		report.MissingCitationCases++
	}
}

func addTrial(metric *Metric, correct bool) {
	metric.Total++
	if correct {
		metric.Correct++
	}
}

func makeMetric(correct, total, pending int) Metric {
	metric := Metric{
		Correct: correct,
		Total:   total,
		Pending: pending,
		State:   "not_applicable",
	}
	if total == 0 {
		return metric
	}
	if pending > 0 {
		metric.State = "pending"
		return metric
	}
	value := float64(correct) / float64(total)
	metric.State = "ready"
	metric.Value = &value
	return metric
}
