package model

import "time"

const (
	KnowledgeGenerationBuilding  = "building"
	KnowledgeGenerationVerified  = "verified"
	KnowledgeGenerationPublished = "published"
	KnowledgeGenerationObsolete  = "obsolete"
	KnowledgeGenerationAbandoned = "abandoned"
)

const (
	KnowledgeTargetPending  = "pending"
	KnowledgeTargetBuilt    = "built"
	KnowledgeTargetVerified = "verified"
)

type KnowledgeIndexGeneration struct {
	ID          string `gorm:"type:char(36);primaryKey;->;<-:create"`
	SourceEpoch uint64 `gorm:"not null;index;->;<-:create"`
	OwnerToken  string `gorm:"type:char(36);not null;uniqueIndex:uk_knowledge_generation_owner;->;<-:create"`
	Attempt     uint32 `gorm:"not null;->;<-:create"`

	Status     string `gorm:"size:24;not null;index"`
	BuildPhase string `gorm:"size:24;not null;default:''"`

	Empty            *bool
	ESIndexName      *string `gorm:"size:191;uniqueIndex:uk_knowledge_generation_es"`
	MilvusCollection *string `gorm:"size:191;uniqueIndex:uk_knowledge_generation_milvus"`

	SourceDatabase   *string    `gorm:"size:128"`
	SourceServerUUID *string    `gorm:"size:64"`
	SnapshotAt       *time.Time `gorm:"type:datetime(3)"`
	ManifestSHA256   *string    `gorm:"type:char(64)"`
	DocumentCount    uint64     `gorm:"not null;default:0"`
	ChunkCount       uint64     `gorm:"not null;default:0"`

	EmbeddingModel         *string `gorm:"size:255"`
	EmbeddingDigest        *string `gorm:"size:255"`
	EmbeddingDimension     *uint32
	EmbeddingServerVersion *string `gorm:"size:255"`
	EmbeddingContextLength *uint32
	EmbeddingInputPolicy   *string `gorm:"size:64"`
	Normalization          *string `gorm:"size:64"`
	ChunkStrategy          *string `gorm:"size:64"`

	ESStatus             string  `gorm:"size:24;not null"`
	MilvusStatus         string  `gorm:"size:24;not null"`
	ESManifestSHA256     *string `gorm:"type:char(64)"`
	MilvusManifestSHA256 *string `gorm:"type:char(64)"`

	SafeErrorCode string     `gorm:"size:64;not null;default:''"`
	VerifiedAt    *time.Time `gorm:"type:datetime(3)"`
	PublishedAt   *time.Time `gorm:"type:datetime(3)"`
	CreatedAt     time.Time  `gorm:"type:datetime(3);not null"`
	UpdatedAt     time.Time  `gorm:"type:datetime(3);not null"`
}
