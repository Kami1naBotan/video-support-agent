package model

type ReplyDecision string

const (
	ReplyAnswer       ReplyDecision = "answer"
	ReplyClarify      ReplyDecision = "clarify"
	ReplyHandoff      ReplyDecision = "handoff"
	ReplyInsufficient ReplyDecision = "insufficient"
)

type GroundedReply struct {
	Decision  ReplyDecision  `json:"decision"`
	Content   string         `json:"content"`
	Citations []RuleCitation `json:"citations"`
}
