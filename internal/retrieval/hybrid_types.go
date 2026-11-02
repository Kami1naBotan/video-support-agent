package retrieval

type HybridOptions struct {
	DocumentID uint64
	VersionID  uint64
	CategoryID uint64

	TopK int
}

type RetrievalSource string

const (
	SourceES     RetrievalSource = "es"
	SourceMilvus RetrievalSource = "milvus"
)

type SourceMatch struct {
	Source   RetrievalSource
	ChunkID  uint64
	Rank     int
	RawScore float64
}

type FusedCandidate struct {
	Candidate Candidate
	RRFScore  float64
	Matches   []SourceMatch
}

type HybridItem struct {
	Rule     HydratedChunk
	ChunkIDs []uint64
	RRFScore float64
	Matches  []SourceMatch
}
