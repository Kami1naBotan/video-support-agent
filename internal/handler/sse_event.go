package handler

type SSEEvent string

const (
	SSEEventStart          SSEEvent = "start"
	SSEEventToken          SSEEvent = "token"
	SSEEventCitation       SSEEvent = "citation"
	SSEEventError          SSEEvent = "error"
	SSEEventDone           SSEEvent = "done"
	SSEEventAgentStart     SSEEvent = "agent_start"
	SSEEventToolStart      SSEEvent = "tool_start"
	SSEEventToolEnd        SSEEvent = "tool_end"
	SSEEventActionRequired SSEEvent = "action_required"
	SSEEventAgentResult    SSEEvent = "agent_result"
)
