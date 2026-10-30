package indexing

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

type rebuildProbe struct {
	t           *testing.T
	ctx         context.Context
	snapshot    *Snapshot
	options     RebuildOptions
	calls       []string
	oldIndex    string
	failAt      string
	failure     error
	publication PublicationResult
	afterVerify func()
}

func newRebuildProbe(t *testing.T, ctx context.Context) *rebuildProbe {
	t.Helper()

	snapshot := contractSnapshot(t)
	options := RebuildOptions{
		NewIndex:           "rule_chunks_v1_test_new",
		Definition:         []byte(`{"settings":{},"mappings":{}}`),
		ExpectedDatabase:   snapshot.SourceDatabase,
		ExpectedServerUUID: snapshot.SourceServerUUID,
	}

	return &rebuildProbe{
		t:        t,
		ctx:      ctx,
		snapshot: snapshot,
		options:  options,
		oldIndex: "rule_chunks_v1_test_old",
		failure:  errors.New("isolated rebuild failure"),
		publication: PublicationResult{
			State:               PublicationApplied,
			ObservedTarget:      options.NewIndex,
			RequestAcknowledged: true,
		},
	}
}

func (p *rebuildProbe) step(ctx context.Context, name string) error {
	p.t.Helper()

	if ctx != p.ctx {
		p.t.Fatalf("%s received a different context", name)
	}
	p.calls = append(p.calls, name)

	if name == p.failAt {
		return p.failure
	}
	return nil
}

func (p *rebuildProbe) source() Source {
	return contractSourceFunc(func(ctx context.Context) (*Snapshot, error) {
		if err := p.step(ctx, "export"); err != nil {
			return nil, err
		}
		return p.snapshot, nil
	})
}

func (p *rebuildProbe) GetAliasTarget(ctx context.Context) (string, error) {
	return p.oldIndex, p.step(ctx, "alias-read")
}

func (p *rebuildProbe) CreateIndex(
	ctx context.Context,
	index string,
	definition []byte,
) error {
	p.t.Helper()

	if index != p.options.NewIndex ||
		string(definition) != string(p.options.Definition) {
		p.t.Fatal("index creation received the wrong target or definition")
	}
	return p.step(ctx, "create")
}

func (p *rebuildProbe) checkDocuments(
	index string,
	documents []EncodedDocument,
) {
	p.t.Helper()

	if index != p.options.NewIndex {
		p.t.Fatal("write or verification targeted the wrong index")
	}
	if documents == nil || len(documents) != len(p.snapshot.Documents) {
		p.t.Fatal("encoded snapshot was lost or truncated")
	}

	for i, encoded := range documents {
		var decoded Document
		if err := json.Unmarshal(encoded.JSON, &decoded); err != nil {
			p.t.Fatal(err)
		}
		if encoded.ID != p.snapshot.Documents[i].ChunkID ||
			!reflect.DeepEqual(decoded, p.snapshot.Documents[i]) {
			p.t.Fatal("encoded document differs from the exported snapshot")
		}
	}
}

func (p *rebuildProbe) BulkIndex(
	ctx context.Context,
	index string,
	documents []EncodedDocument,
) error {
	p.checkDocuments(index, documents)
	return p.step(ctx, "bulk")
}

func (p *rebuildProbe) VerifyIndex(
	ctx context.Context,
	index string,
	documents []EncodedDocument,
) error {
	p.checkDocuments(index, documents)
	err := p.step(ctx, "verify")
	if p.afterVerify != nil {
		p.afterVerify()
	}
	return err
}

func (p *rebuildProbe) SwitchAlias(
	ctx context.Context,
	oldIndex string,
	newIndex string,
) (PublicationResult, error) {
	p.t.Helper()

	if oldIndex != p.oldIndex || newIndex != p.options.NewIndex {
		p.t.Fatal("publication received incorrect old/new targets")
	}
	return p.publication, p.step(ctx, "publish")
}

func requireRebuildCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("operation order:\nwant %v\ngot  %v", want, got)
	}
}

func TestRebuildSuccess(t *testing.T) {
	for _, oldIndex := range []string{"rule_chunks_v1_test_old", ""} {
		name := "existing_alias"
		if oldIndex == "" {
			name = "missing_alias"
		}

		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			probe := newRebuildProbe(t, ctx)
			probe.oldIndex = oldIndex

			result, err := Rebuild(ctx, probe.source(), probe, probe.options)
			if err != nil {
				t.Fatal(err)
			}
			requireRebuildCalls(t, probe.calls, []string{
				"export", "alias-read", "create", "bulk", "verify", "publish",
			})

			if result == nil ||
				result.Stage != "complete" ||
				result.OldIndex != oldIndex ||
				result.NewIndex != probe.options.NewIndex ||
				result.DocumentCount != len(probe.snapshot.Documents) ||
				result.SourceDatabase != probe.snapshot.SourceDatabase ||
				result.SourceServerUUID != probe.snapshot.SourceServerUUID ||
				result.CapturedAt != probe.snapshot.CapturedAt.UTC().
					Truncate(time.Millisecond) ||
				result.Publication != probe.publication {
				t.Fatalf("incorrect rebuild result: %+v", result)
			}

			var expectedBytes int64
			for _, document := range probe.snapshot.Documents {
				body, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				expectedBytes += int64(len(body))
			}
			if result.TotalJSONBytes != expectedBytes {
				t.Fatal("rebuild result lost exact JSON byte count")
			}
		})
	}
}

func TestRebuildStopsAtEveryFailure(t *testing.T) {
	stages := []string{
		"export", "alias-read", "create", "bulk", "verify", "publish",
	}

	for position, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			probe := newRebuildProbe(t, ctx)
			probe.failAt = stage
			probe.publication = PublicationResult{
				State: PublicationUnknown,
			}

			result, err := Rebuild(ctx, probe.source(), probe, probe.options)
			if !errors.Is(err, probe.failure) {
				t.Fatalf("original error was lost: %v", err)
			}
			if result == nil || result.Stage != stage {
				t.Fatalf("failure stage was lost: %+v", result)
			}
			requireRebuildCalls(t, probe.calls, stages[:position+1])

			if stage != "publish" &&
				result.Publication.State != PublicationNotAttempted {
				t.Fatal("failure before publication changed publication state")
			}
			if stage == "publish" &&
				result.Publication.State != PublicationUnknown {
				t.Fatal("publication uncertainty was lost")
			}
		})
	}
}

func TestRebuildSourceGuards(t *testing.T) {
	cases := []struct {
		name   string
		change func(*rebuildProbe)
	}{
		{"database_mismatch", func(p *rebuildProbe) {
			p.options.ExpectedDatabase = "another_database"
		}},
		{"server_mismatch", func(p *rebuildProbe) {
			p.options.ExpectedServerUUID = "another_server"
		}},
		{"empty_without_authorization", func(p *rebuildProbe) {
			p.snapshot.Documents = []Document{}
		}},
		{"allow_empty_does_not_bypass_identity", func(p *rebuildProbe) {
			p.snapshot.Documents = []Document{}
			p.options.AllowEmpty = true
			p.options.ExpectedServerUUID = "another_server"
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			probe := newRebuildProbe(t, ctx)
			tc.change(probe)

			result, err := Rebuild(ctx, probe.source(), probe, probe.options)
			if !errors.Is(err, ErrInvalidSource) ||
				result == nil ||
				result.Stage != "source-check" ||
				result.Publication.State != PublicationNotAttempted {
				t.Fatalf("source guard failed: result=%+v error=%v", result, err)
			}
			requireRebuildCalls(t, probe.calls, []string{"export"})
		})
	}

	t.Run("explicit_empty", func(t *testing.T) {
		ctx := context.Background()
		probe := newRebuildProbe(t, ctx)
		probe.snapshot.Documents = []Document{}
		probe.options.AllowEmpty = true

		result, err := Rebuild(ctx, probe.source(), probe, probe.options)
		if err != nil {
			t.Fatal(err)
		}
		if result.DocumentCount != 0 ||
			result.TotalJSONBytes != 0 ||
			result.Stage != "complete" {
			t.Fatalf("incorrect empty rebuild result: %+v", result)
		}
		requireRebuildCalls(t, probe.calls, []string{
			"export", "alias-read", "create", "bulk", "verify", "publish",
		})
	})

	t.Run("new_target_is_current", func(t *testing.T) {
		ctx := context.Background()
		probe := newRebuildProbe(t, ctx)
		probe.oldIndex = probe.options.NewIndex

		result, err := Rebuild(ctx, probe.source(), probe, probe.options)
		if !errors.Is(err, ErrInvalidSource) ||
			result.Publication.State != PublicationNotAttempted {
			t.Fatalf("current index overwrite was not rejected: %v", err)
		}
		requireRebuildCalls(t, probe.calls, []string{"export", "alias-read"})
	})
}

func TestRebuildPreflightAndCancellation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*RebuildOptions)
	}{
		{"missing_database", func(o *RebuildOptions) {
			o.ExpectedDatabase = ""
		}},
		{"missing_server", func(o *RebuildOptions) {
			o.ExpectedServerUUID = ""
		}},
		{"identity_whitespace", func(o *RebuildOptions) {
			o.ExpectedDatabase += " "
		}},
		{"missing_index", func(o *RebuildOptions) {
			o.NewIndex = ""
		}},
		{"missing_definition", func(o *RebuildOptions) {
			o.Definition = nil
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			probe := newRebuildProbe(t, ctx)
			tc.change(&probe.options)

			result, err := Rebuild(ctx, probe.source(), probe, probe.options)
			if !errors.Is(err, ErrInvalidSource) ||
				result == nil || result.Stage != "preflight" ||
				len(probe.calls) != 0 {
				t.Fatalf("invalid preflight performed work: %v", err)
			}
		})
	}

	t.Run("nil_dependencies", func(t *testing.T) {
		ctx := context.Background()
		probe := newRebuildProbe(t, ctx)

		if _, err := Rebuild(ctx, nil, probe, probe.options); !errors.Is(err, ErrInvalidSource) {
			t.Fatal("nil source accepted")
		}
		if _, err := Rebuild(ctx, probe.source(), nil, probe.options); !errors.Is(err, ErrInvalidSource) {
			t.Fatal("nil writer accepted")
		}
		if len(probe.calls) != 0 {
			t.Fatal("nil dependency caused work")
		}
	})

	t.Run("already_canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		probe := newRebuildProbe(t, ctx)

		result, err := Rebuild(ctx, probe.source(), probe, probe.options)
		if !errors.Is(err, context.Canceled) ||
			result.Publication.State != PublicationNotAttempted ||
			len(probe.calls) != 0 {
			t.Fatal("already canceled rebuild performed work")
		}
	})

	t.Run("canceled_after_verification", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		probe := newRebuildProbe(t, ctx)
		probe.afterVerify = cancel

		result, err := Rebuild(ctx, probe.source(), probe, probe.options)
		if !errors.Is(err, context.Canceled) ||
			result.Publication.State != PublicationNotAttempted {
			t.Fatalf("cancellation before publication was ignored: %v", err)
		}
		requireRebuildCalls(t, probe.calls, []string{
			"export", "alias-read", "create", "bulk", "verify",
		})
	})
}

func TestRebuildPublicationOutcome(t *testing.T) {
	t.Run("applied_with_error", func(t *testing.T) {
		ctx := context.Background()
		probe := newRebuildProbe(t, ctx)
		probe.failAt = "publish"
		probe.failure = context.DeadlineExceeded
		probe.publication.RequestAcknowledged = false

		result, err := Rebuild(ctx, probe.source(), probe, probe.options)
		if !errors.Is(err, context.DeadlineExceeded) ||
			result.Stage != "publish" ||
			result.Publication != probe.publication {
			t.Fatalf("applied state or request error lost: %+v, %v", result, err)
		}
	})

	cases := []struct {
		name   string
		state  PublicationState
		target string
	}{
		{"unknown", PublicationUnknown, ""},
		{"old_observed", PublicationOldObserved, "rule_chunks_v1_test_old"},
		{"wrong_target", PublicationApplied, "unexpected_target"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			probe := newRebuildProbe(t, ctx)
			probe.publication = PublicationResult{
				State:          tc.state,
				ObservedTarget: tc.target,
			}

			result, err := Rebuild(ctx, probe.source(), probe, probe.options)
			if err == nil || result.Stage == "complete" ||
				result.Publication != probe.publication {
				t.Fatalf("unconfirmed publication reported success: %+v", result)
			}
		})
	}
}
