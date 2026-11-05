package generationquery

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"video-support-agent/internal/embedding"
	"video-support-agent/internal/retrieval"
)

var (
	ErrTarget  = errors.New("invalid generation query target")
	ErrResolve = errors.New("generation query target unavailable")

	idPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Target struct {
	GenerationID     string
	Epoch            uint64
	Empty            bool
	ESIndex          string
	MilvusCollection string
	ManifestSHA256   string
	Identity         embedding.ModelIdentity
}

func (t Target) Validate() error {
	if !idPattern.MatchString(t.GenerationID) || t.Epoch == 0 ||
		!hashPattern.MatchString(t.ManifestSHA256) ||
		t.Identity.Model != embedding.ModelName ||
		!hashPattern.MatchString(t.Identity.Digest) ||
		t.Identity.Dimensions != embedding.Dimensions ||
		t.Identity.ContextLength != embedding.ContextLength ||
		t.Identity.InputPolicy != embedding.InputPolicy ||
		t.Identity.ServerVersion == "" ||
		t.Identity.ServerVersion != strings.TrimSpace(t.Identity.ServerVersion) {
		return ErrTarget
	}

	if t.Empty {
		if t.ESIndex != "" || t.MilvusCollection != "" {
			return ErrTarget
		}
		return nil
	}

	suffix := strings.ReplaceAll(t.GenerationID, "-", "")
	if t.ESIndex != "knowledge_g_"+suffix || t.MilvusCollection != "rule_chunks_vec_v1_"+suffix {
		return ErrTarget
	}
	return nil
}

type Resolver interface {
	ResolveActive(context.Context) (Target, error)
}

type ESSearcher interface {
	SearchCandidatesAt(context.Context, string, string, retrieval.SearchOptions, time.Time) ([]retrieval.Candidate, error)
}

type VectorSearcher interface {
	SearchCandidatesAt(context.Context, string, embedding.ModelIdentity, string, retrieval.SearchOptions, time.Time) ([]retrieval.Candidate, error)
}

type Pair struct {
	GenerationID string
	Epoch        uint64
	At           time.Time
	ES           retrieval.CandidateSearcher
	Vector       retrieval.CandidateSearcher
}

func Bind(ctx context.Context, resolver Resolver, es ESSearcher, vector VectorSearcher, at time.Time) (*Pair, error) {
	if resolver == nil || es == nil || vector == nil ||
		at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return nil, ErrTarget
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	target, err := resolver.ResolveActive(ctx)
	if err != nil {
		return nil, err
	}
	if err := target.Validate(); err != nil {
		return nil, err
	}

	at = at.UTC().Truncate(time.Millisecond)
	return &Pair{
		GenerationID: target.GenerationID,
		Epoch:        target.Epoch,
		At:           at,
		ES: &boundSearcher{
			target: target, at: at, es: es,
		},
		Vector: &boundSearcher{
			target: target, at: at, vector: vector,
		},
	}, nil
}

type boundSearcher struct {
	target Target
	at     time.Time
	es     ESSearcher
	vector VectorSearcher
}

func (b *boundSearcher) SearchCandidates(ctx context.Context, query string, opts retrieval.SearchOptions, at time.Time) ([]retrieval.Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !at.UTC().Truncate(time.Millisecond).Equal(b.at) {
		return nil, ErrTarget
	}

	normalized, options, err := retrieval.NormalizeSearch(query, opts)
	if err != nil {
		return nil, err
	}

	if b.vector != nil {
		if len(query) > embedding.MaxTextBytes || normalized == "" || options.Section != "" || options.ArticleNumber != "" || options.Highlight {
			return nil, retrieval.ErrInvalidQuery
		}
	}

	if b.target.Empty {
		return []retrieval.Candidate{}, nil
	}

	if b.vector != nil {
		return b.vector.SearchCandidatesAt(
			ctx, b.target.MilvusCollection, b.target.Identity,
			normalized, options, b.at,
		)
	}
	return b.es.SearchCandidatesAt(
		ctx, b.target.ESIndex, normalized, options, b.at,
	)
}
