package tools

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"video-support-agent/internal/service"
)

func DecodeFlatObject(raw string, dst any, allowed ...string) error {
	return fail(decodeObject(raw, dst, allowed...))
}

func NormalizeCall(name, raw string) (string, error) {
	var value any

	switch name {
	case SearchRuleName:
		var args searchRuleArgs
		if err := decodeObject(raw, &args, "query", "top_k"); err != nil {
			return "", fail(err)
		}

		args.Query = strings.TrimSpace(args.Query)
		if args.Query == "" || utf8.RuneCountInString(args.Query) > 200 {
			return "", fail(service.ErrInvalidArgument)
		}

		topK := 3
		if args.TopK != nil {
			topK = *args.TopK
		}
		if topK < 1 || topK > 3 {
			return "", fail(service.ErrInvalidArgument)
		}

		args.TopK = &topK
		value = args

	case GetRuleDetailName:
		var args ruleDetailArgs
		if err := decodeObject(raw, &args, "mode", "document_id", "version_id", "article_id"); err != nil {
			return "", fail(err)
		}

		values := map[string]string{"mode": args.Mode}
		var ids []*string

		switch args.Mode {
		case "current":
			if args.DocumentID == nil || args.VersionID != nil || args.ArticleID != nil {
				return "", fail(service.ErrInvalidArgument)
			}

			ids = append(ids, args.DocumentID)
			values["document_id"] = *args.DocumentID

		case "history":
			if args.DocumentID != nil || args.VersionID == nil || args.ArticleID == nil {
				return "", fail(service.ErrInvalidArgument)
			}

			ids = append(ids, args.VersionID, args.ArticleID)
			values["version_id"] = *args.VersionID
			values["article_id"] = *args.ArticleID

		default:
			return "", fail(service.ErrInvalidArgument)
		}

		for _, id := range ids {
			if _, err := parseID(*id); err != nil {
				return "", fail(err)
			}
		}

		value = values

	case CheckAppealEligibilityName:
		var facts service.AppealFacts
		if err := decodeObject(raw, &facts, "subject_type", "subject_ref", "reason"); err != nil {
			return "", fail(err)
		}

		normalized, _, err := service.NormalizeAppealFacts(facts)
		if err != nil {
			return "", fail(err)
		}

		value = normalized

	case CreateAppealTicketName:
		input, err := decodeCreateAppeal(raw)
		if err != nil {
			return "", fail(err)
		}

		value = input

	case QueryTicketStatusName:
		var args queryTicketArgs
		if err := decodeObject(raw, &args, "ticket_id"); err != nil {
			return "", fail(err)
		}

		if _, err := parseID(args.TicketID); err != nil {
			return "", fail(err)
		}

		value = args

	default:
		return "", fail(service.ErrInvalidArgument)
	}

	data, err := json.Marshal(value)
	if err != nil || len(data) > MaxArgumentBytes {
		return "", fail(service.ErrInvalidArgument)
	}

	return string(data), nil
}
