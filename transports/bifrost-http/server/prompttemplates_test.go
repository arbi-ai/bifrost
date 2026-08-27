package server

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/stretchr/testify/require"
)

// fakePromptConfigStore serves prompt versions and nothing else. Embedding the
// interface leaves every other method nil, which is the point: a test that
// reached one would panic loudly rather than quietly exercise real storage.
type fakePromptConfigStore struct {
	configstore.ConfigStore
	latest   *configstoreTables.TablePromptVersion
	versions []configstoreTables.TablePromptVersion
	err      error
}

func (f *fakePromptConfigStore) GetLatestPromptVersion(_ context.Context, _ string) (*configstoreTables.TablePromptVersion, error) {
	return f.latest, f.err
}

func (f *fakePromptConfigStore) GetPromptVersions(_ context.Context, _ string) ([]configstoreTables.TablePromptVersion, error) {
	return f.versions, f.err
}

func chatMessageJSON(t *testing.T, role schemas.ChatMessageRole, text string) []byte {
	t.Helper()
	data, err := schemas.Marshal(schemas.ChatMessage{
		Role:    role,
		Content: &schemas.ChatMessageContent{ContentStr: &text},
	})
	require.NoError(t, err)
	return data
}

func versionRow(number int, messages ...configstoreTables.TablePromptVersionMessage) *configstoreTables.TablePromptVersion {
	return &configstoreTables.TablePromptVersion{
		VersionNumber: number,
		Messages:      messages,
	}
}

func TestResolveVersionZeroTakesLatest(t *testing.T) {
	row := versionRow(7, configstoreTables.TablePromptVersionMessage{
		OrderIndex: 0,
		Message:    chatMessageJSON(t, schemas.ChatMessageRoleSystem, "You help {{ tier }} customers."),
	})
	s := NewPromptTemplatesStore(&fakePromptConfigStore{latest: row})

	got, err := s.ResolveVersion(context.Background(), "support-reply", 0)
	require.NoError(t, err)

	// The CONCRETE number, never the requested 0: the module keys its template
	// cache on it, and caching under 0 would serve a stale compiled template
	// forever after a new version is published.
	require.Equal(t, 7, got.Version)
	require.Equal(t, "support-reply", got.PromptID)
	require.Len(t, got.Messages, 1)
	require.Equal(t, "system", got.Messages[0].Role)
	require.Equal(t, "You help {{ tier }} customers.", got.Messages[0].Content)
}

func TestResolveVersionExplicitSelectsThatVersion(t *testing.T) {
	s := NewPromptTemplatesStore(&fakePromptConfigStore{versions: []configstoreTables.TablePromptVersion{
		*versionRow(1, configstoreTables.TablePromptVersionMessage{
			Message: chatMessageJSON(t, schemas.ChatMessageRoleSystem, "old"),
		}),
		*versionRow(3, configstoreTables.TablePromptVersionMessage{
			Message: chatMessageJSON(t, schemas.ChatMessageRoleSystem, "new"),
		}),
	}})

	got, err := s.ResolveVersion(context.Background(), "support-reply", 3)
	require.NoError(t, err)
	require.Equal(t, 3, got.Version)
	require.Equal(t, "new", got.Messages[0].Content)
}

func TestResolveVersionReportsAnUnknownVersion(t *testing.T) {
	s := NewPromptTemplatesStore(&fakePromptConfigStore{versions: []configstoreTables.TablePromptVersion{
		*versionRow(1),
	}})
	_, err := s.ResolveVersion(context.Background(), "support-reply", 9)
	require.Error(t, err)
	require.Contains(t, err.Error(), "version 9")
}

func TestResolveVersionReportsAPromptWithNoVersions(t *testing.T) {
	s := NewPromptTemplatesStore(&fakePromptConfigStore{latest: nil})
	_, err := s.ResolveVersion(context.Background(), "support-reply", 0)
	require.Error(t, err)
}

// Declared variables with EMPTY defaults must survive the widening.
//
// The prompt_versions table stores declared names with empty values.
// render.Merge drops empty defaults so StrictUndefined still fires for a
// declared-but-unsupplied variable, while render.DeclaredNames still lists the
// name so a client message may reference it. Dropping the key here would defeat
// both halves at once.
func TestDeclaredVariablesKeepEmptyDefaults(t *testing.T) {
	row := versionRow(1, configstoreTables.TablePromptVersionMessage{
		Message: chatMessageJSON(t, schemas.ChatMessageRoleSystem, "hi"),
	})
	row.Variables = configstoreTables.PromptVariables{"tier": "", "region": "emea"}
	s := NewPromptTemplatesStore(&fakePromptConfigStore{latest: row})

	got, err := s.ResolveVersion(context.Background(), "support-reply", 0)
	require.NoError(t, err)
	require.Contains(t, got.Variables, "tier", "a declared-but-empty name must survive")
	require.Equal(t, "", got.Variables["tier"])
	require.Equal(t, "emea", got.Variables["region"])
}

func TestModelParamsArePassedThrough(t *testing.T) {
	row := versionRow(1, configstoreTables.TablePromptVersionMessage{
		Message: chatMessageJSON(t, schemas.ChatMessageRoleSystem, "hi"),
	})
	row.ModelParams = configstoreTables.ModelParams{"temperature": 0.5}
	s := NewPromptTemplatesStore(&fakePromptConfigStore{latest: row})

	got, err := s.ResolveVersion(context.Background(), "support-reply", 0)
	require.NoError(t, err)
	require.InDelta(t, 0.5, got.ModelParams["temperature"], 1e-9)
}

// --- the stored envelope -----------------------------------------------------

// The prompt repository stores messages as opaque JSON in several shapes. These
// mirror the built-in prompts plugin rather than inventing a second reading of
// the same rows.
func TestDecodesTheCompletionResultEnvelope(t *testing.T) {
	raw := []byte(`{"originalType":"completion_result","payload":{"choices":[{"message":{"role":"assistant","content":"from {{ x }}"}}]}}`)
	row := versionRow(1, configstoreTables.TablePromptVersionMessage{Message: raw})
	s := NewPromptTemplatesStore(&fakePromptConfigStore{latest: row})

	got, err := s.ResolveVersion(context.Background(), "p", 0)
	require.NoError(t, err)
	require.Equal(t, "assistant", got.Messages[0].Role)
	require.Equal(t, "from {{ x }}", got.Messages[0].Content)
}

func TestDecodesADirectPayloadEnvelope(t *testing.T) {
	raw := []byte(`{"originalType":"completion_request","payload":{"role":"system","content":"direct {{ x }}"}}`)
	row := versionRow(1, configstoreTables.TablePromptVersionMessage{Message: raw})
	s := NewPromptTemplatesStore(&fakePromptConfigStore{latest: row})

	got, err := s.ResolveVersion(context.Background(), "p", 0)
	require.NoError(t, err)
	require.Equal(t, "system", got.Messages[0].Role)
	require.Equal(t, "direct {{ x }}", got.Messages[0].Content)
}

// Rows written before the envelope existed are bare messages.
func TestDecodesABareChatMessage(t *testing.T) {
	row := versionRow(1, configstoreTables.TablePromptVersionMessage{
		Message: chatMessageJSON(t, schemas.ChatMessageRoleUser, "bare {{ x }}"),
	})
	s := NewPromptTemplatesStore(&fakePromptConfigStore{latest: row})

	got, err := s.ResolveVersion(context.Background(), "p", 0)
	require.NoError(t, err)
	require.Equal(t, "bare {{ x }}", got.Messages[0].Content)
}

// MessageJSON is the fallback column when Message bytes are absent.
func TestFallsBackToTheMessageJSONColumn(t *testing.T) {
	row := versionRow(1, configstoreTables.TablePromptVersionMessage{
		MessageJSON: string(chatMessageJSON(t, schemas.ChatMessageRoleSystem, "from column")),
	})
	s := NewPromptTemplatesStore(&fakePromptConfigStore{latest: row})

	got, err := s.ResolveVersion(context.Background(), "p", 0)
	require.NoError(t, err)
	require.Equal(t, "from column", got.Messages[0].Content)
}

// --- content blocks ----------------------------------------------------------

func TestJoinsTextContentBlocks(t *testing.T) {
	first, second := "line one {{ x }}", "line two"
	data, err := schemas.Marshal(schemas.ChatMessage{
		Role: schemas.ChatMessageRoleSystem,
		Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
			{Type: schemas.ChatContentBlockTypeText, Text: &first},
			{Type: schemas.ChatContentBlockTypeText, Text: &second},
		}},
	})
	require.NoError(t, err)

	row := versionRow(1, configstoreTables.TablePromptVersionMessage{Message: data})
	s := NewPromptTemplatesStore(&fakePromptConfigStore{latest: row})

	got, err := s.ResolveVersion(context.Background(), "p", 0)
	require.NoError(t, err)
	require.Equal(t, "line one {{ x }}\nline two", got.Messages[0].Content)
}

// A non-text block is a LOUD failure, not a silent drop. This store shape
// carries text only, and quietly discarding an image from a stored prompt would
// change what the model sees with no signal anywhere.
func TestRejectsANonTextContentBlock(t *testing.T) {
	data, err := schemas.Marshal(schemas.ChatMessage{
		Role: schemas.ChatMessageRoleUser,
		Content: &schemas.ChatMessageContent{ContentBlocks: []schemas.ChatContentBlock{
			{Type: schemas.ChatContentBlockTypeImage, ImageURLStruct: &schemas.ChatInputImage{URL: "https://x/y"}},
		}},
	})
	require.NoError(t, err)

	row := versionRow(1, configstoreTables.TablePromptVersionMessage{Message: data})
	s := NewPromptTemplatesStore(&fakePromptConfigStore{latest: row})

	_, resolveErr := s.ResolveVersion(context.Background(), "p", 0)
	require.Error(t, resolveErr)
	require.Contains(t, resolveErr.Error(), "text only")
}

// --- registration ------------------------------------------------------------

// The list feeds the module's coexistence check, which refuses to construct
// when prompts is also enabled. A disabled entry must not trip it.
func TestEnabledPluginNamesListsOnlyEnabledPlugins(t *testing.T) {
	cfg := &lib.Config{PluginConfigs: []*schemas.PluginConfig{
		{Name: "governance", Enabled: true},
		{Name: "prompts", Enabled: false},
		{Name: "semanticcache", Enabled: true},
		nil,
	}}
	require.Equal(t, []string{"governance", "semanticcache"}, enabledPluginNames(cfg))
}
