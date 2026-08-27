package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/arbi-ai/bifrost-prompt-templates/partials"
	prompttemplates "github.com/arbi-ai/bifrost-prompt-templates/store"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
)

// PromptTemplatesStore adapts the framework config store to the prompt-templates
// module's store interfaces.
//
// The module deliberately does not import Bifrost's persistence types, so this
// adapter is the only place the two vocabularies meet. It lives in the fork
// rather than the module for the same reason: the module has to build as a .so
// where these tables do not exist.
type PromptTemplatesStore struct {
	store configstore.ConfigStore
}

// NewPromptTemplatesStore builds the adapter.
func NewPromptTemplatesStore(store configstore.ConfigStore) *PromptTemplatesStore {
	return &PromptTemplatesStore{store: store}
}

// ResolveVersion implements prompttemplates.PromptStore.
//
// version 0 means "latest". The returned Version is always the CONCRETE number:
// the module keys its template cache on it, and keying on the requested 0 would
// serve a stale compiled template forever after a new version is published.
func (s *PromptTemplatesStore) ResolveVersion(ctx context.Context, promptID string, version int) (*prompttemplates.PromptVersion, error) {
	if s.store == nil {
		return nil, fmt.Errorf("prompt store is not configured")
	}

	row, err := s.resolveRow(ctx, promptID, version)
	if err != nil {
		return nil, err
	}

	messages, err := storedMessages(promptID, row.Messages)
	if err != nil {
		return nil, err
	}

	return &prompttemplates.PromptVersion{
		PromptID: promptID,
		Version:  row.VersionNumber,
		// The OSS config store has no tenant column. An empty tenant is still a
		// correct cache-key component — prompt ID already separates callers
		// here — and the field is populated by deployments that add one.
		TenantID:    "",
		Messages:    messages,
		ModelParams: map[string]any(row.ModelParams),
		Variables:   declaredVariables(row.Variables),
	}, nil
}

func (s *PromptTemplatesStore) resolveRow(ctx context.Context, promptID string, version int) (*configstoreTables.TablePromptVersion, error) {
	if version <= 0 {
		row, err := s.store.GetLatestPromptVersion(ctx, promptID)
		if err != nil {
			return nil, fmt.Errorf("loading latest version of prompt %q: %w", promptID, err)
		}
		if row == nil {
			return nil, fmt.Errorf("prompt %q has no versions", promptID)
		}
		return row, nil
	}

	rows, err := s.store.GetPromptVersions(ctx, promptID)
	if err != nil {
		return nil, fmt.Errorf("loading versions of prompt %q: %w", promptID, err)
	}
	for i := range rows {
		if rows[i].VersionNumber == version {
			return &rows[i], nil
		}
	}
	return nil, fmt.Errorf("prompt %q has no version %d", promptID, version)
}

// declaredVariables widens the stored map[string]string to the map[string]any
// the renderer takes.
//
// Empty values are KEPT. The prompt_versions table stores declared names with
// empty defaults, and render.Merge drops empty defaults precisely so that
// StrictUndefined still fires for a declared-but-unsupplied variable — while
// render.DeclaredNames still lists the name so a client message may reference
// it. Dropping them here would defeat both.
func declaredVariables(vars configstoreTables.PromptVariables) map[string]any {
	if len(vars) == 0 {
		return nil
	}
	out := make(map[string]any, len(vars))
	for name, value := range vars {
		out[name] = value
	}
	return out
}

// storedMessages decodes each stored row into the module's flat message shape.
func storedMessages(promptID string, rows []configstoreTables.TablePromptVersionMessage) ([]prompttemplates.Message, error) {
	out := make([]prompttemplates.Message, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		data := []byte(row.Message)
		if len(data) == 0 && row.MessageJSON != "" {
			data = []byte(row.MessageJSON)
		}

		chatMessage, err := decodePromptMessage(data)
		if err != nil {
			return nil, fmt.Errorf("prompt %q message %d: %w", promptID, row.OrderIndex, err)
		}

		text, err := messageText(chatMessage)
		if err != nil {
			return nil, fmt.Errorf("prompt %q message %d: %w", promptID, row.OrderIndex, err)
		}

		out = append(out, prompttemplates.Message{
			Role:    string(chatMessage.Role),
			Content: text,
		})
	}
	return out, nil
}

// messageText flattens a stored message to the template source.
//
// A message whose content is carried in blocks is joined when every block is
// text. A non-text block is an ERROR rather than a silent drop: this store
// shape carries text only, and quietly discarding an image from a stored prompt
// would change what the model sees with no signal anywhere.
func messageText(m schemas.ChatMessage) (string, error) {
	if m.Content == nil {
		return "", nil
	}
	if m.Content.ContentStr != nil {
		return *m.Content.ContentStr, nil
	}

	var parts []string
	for _, block := range m.Content.ContentBlocks {
		if block.Type != schemas.ChatContentBlockTypeText || block.Text == nil {
			return "", fmt.Errorf(
				"content block of type %q is not supported in a stored template; text only",
				block.Type)
		}
		parts = append(parts, *block.Text)
	}
	return strings.Join(parts, "\n"), nil
}

// decodePromptMessage unwraps the stored envelope.
//
// The prompt repository stores messages as opaque JSON in one of several
// shapes, so this mirrors the built-in prompts plugin rather than inventing a
// second reading of the same rows: an envelope with originalType/payload
// (payload being a completion result to take choices[0].message from, or a
// direct ChatMessage), or a bare ChatMessage.
func decodePromptMessage(data []byte) (schemas.ChatMessage, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return schemas.ChatMessage{}, fmt.Errorf("empty message")
	}
	data = []byte(trimmed)

	var envelope struct {
		OriginalType string          `json:"originalType"`
		Payload      json.RawMessage `json:"payload"`
	}
	if err := schemas.Unmarshal(data, &envelope); err == nil {
		payload := strings.TrimSpace(string(envelope.Payload))
		if payload != "" && payload != "null" {
			if envelope.OriginalType == "completion_result" {
				var result struct {
					Choices []struct {
						Message *schemas.ChatMessage `json:"message"`
					} `json:"choices"`
				}
				if err := schemas.Unmarshal([]byte(payload), &result); err == nil &&
					len(result.Choices) > 0 && result.Choices[0].Message != nil &&
					messagePopulated(*result.Choices[0].Message) {
					return *result.Choices[0].Message, nil
				}
			}

			var message schemas.ChatMessage
			if err := schemas.Unmarshal([]byte(payload), &message); err != nil {
				return schemas.ChatMessage{}, fmt.Errorf("decoding prompt message envelope payload: %w", err)
			}
			if messagePopulated(message) {
				return message, nil
			}
		}
	}

	var message schemas.ChatMessage
	if err := schemas.Unmarshal(data, &message); err != nil {
		return schemas.ChatMessage{}, err
	}
	return message, nil
}

// messagePopulated reports whether a decoded message carries anything worth
// injecting.
//
// It reads only NAMED fields. ChatMessage promotes Refusal, ToolCalls and
// ToolCallID through embedded pointers, and merely reading one of those on a
// plain message is a nil dereference — so the embeddings themselves are what is
// checked.
func messagePopulated(m schemas.ChatMessage) bool {
	if strings.TrimSpace(string(m.Role)) != "" {
		return true
	}
	if m.Content != nil {
		return true
	}
	if m.Name != nil && strings.TrimSpace(*m.Name) != "" {
		return true
	}
	return m.ChatToolMessage != nil || m.ChatAssistantMessage != nil
}

// PromptTemplatesPartialStore is a placeholder partial source.
//
// Reusable fragments need a prompt_partials table and its CRUD, which is a
// separate piece of work. Until then every tenant has an empty partial set,
// which is a valid Set: templates simply cannot {% include %} anything.
type PromptTemplatesPartialStore struct{}

// PartialsFor implements prompttemplates.PartialStore.
func (PromptTemplatesPartialStore) PartialsFor(context.Context, string) ([]partials.Partial, error) {
	return nil, nil
}
