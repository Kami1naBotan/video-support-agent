package handler

type SSEEvent string

const (
	SSEEventStart    SSEEvent = "start"
	SSEEventToken    SSEEvent = "token"
	SSEEventCitation SSEEvent = "citation"
	SSEEventError    SSEEvent = "error"
	SSEEventDone     SSEEvent = "done"
)
