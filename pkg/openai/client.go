package openai

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/acorn-io/gptscript/pkg/cache"
	"github.com/acorn-io/gptscript/pkg/hash"
	"github.com/acorn-io/gptscript/pkg/types"
	"github.com/acorn-io/gptscript/pkg/vision"
	"github.com/sashabaranov/go-openai"
)

const (
	DefaultVisionModel     = openai.GPT4VisionPreview
	DefaultModel           = openai.GPT4TurboPreview
	DefaultMaxTokens       = 1024
	DefaultPromptParameter = "defaultPromptParameter"
)

var (
	key           = os.Getenv("OPENAI_API_KEY")
	url           = os.Getenv("OPENAI_URL")
	transactionID int64
)

type Client struct {
	url   string
	key   string
	c     *openai.Client
	cache *cache.Client
}

type Options struct {
	BaseURL    string         `usage:"OpenAI base URL" name:"openai-base-url" env:"OPENAI_BASE_URL"`
	APIKey     string         `usage:"OpenAI API KEY" name:"openai-api-key" env:"OPENAI_API_KEY"`
	APIVersion string         `usage:"OpenAI API Version (for Azure)" name:"openai-api-version" env:"OPENAI_API_VERSION"`
	APIType    openai.APIType `usage:"OpenAI API Type (valid: OPEN_AI, AZURE, AZURE_AD)" name:"openai-api-type" env:"OPENAI_API_TYPE"`
	OrgID      string         `usage:"OpenAI organization ID" name:"openai-org-id" env:"OPENAI_ORG_ID"`
	Cache      *cache.Client
}

func complete(opts ...Options) (result Options, err error) {
	for _, opt := range opts {
		result.BaseURL = types.FirstSet(opt.BaseURL, result.BaseURL)
		result.APIKey = types.FirstSet(opt.APIKey, result.APIKey)
		result.OrgID = types.FirstSet(opt.OrgID, result.OrgID)
		result.Cache = types.FirstSet(opt.Cache, result.Cache)
		result.APIVersion = types.FirstSet(opt.APIVersion, result.APIVersion)
		result.APIType = types.FirstSet(opt.APIType, result.APIType)
	}

	if result.Cache == nil {
		result.Cache, err = cache.New(cache.Options{
			Cache: new(bool),
		})
	}

	if result.BaseURL == "" && url != "" {
		result.BaseURL = url
	}

	if result.APIKey == "" && key != "" {
		result.APIKey = key
	}

	return result, err
}

func NewClient(opts ...Options) (*Client, error) {
	opt, err := complete(opts...)
	if err != nil {
		return nil, err
	}

	cfg := openai.DefaultConfig(opt.APIKey)
	if strings.Contains(string(opt.APIType), "AZURE") {
		cfg = openai.DefaultAzureConfig(key, url)
	}

	cfg.BaseURL = types.FirstSet(opt.BaseURL, cfg.BaseURL)
	cfg.OrgID = types.FirstSet(opt.OrgID, cfg.OrgID)
	cfg.APIVersion = types.FirstSet(opt.APIVersion, cfg.APIVersion)
	cfg.APIType = types.FirstSet(opt.APIType, cfg.APIType)

	return &Client{
		c:     openai.NewClientWithConfig(cfg),
		cache: opt.Cache,
	}, nil
}

func (c *Client) ListModules(ctx context.Context) (result []string, _ error) {
	models, err := c.c.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, model := range models.Models {
		result = append(result, model.ID)
	}
	sort.Strings(result)
	return result, nil
}

func (c *Client) cacheKey(request openai.ChatCompletionRequest) string {
	return hash.Encode(map[string]any{
		"url":     c.url,
		"key":     c.key,
		"request": request,
	})
}

func (c *Client) seed(request openai.ChatCompletionRequest) int {
	newRequest := request
	newRequest.Messages = nil

	for _, msg := range request.Messages {
		newMsg := msg
		newMsg.ToolCalls = nil
		newMsg.ToolCallID = ""

		for _, tool := range msg.ToolCalls {
			tool.ID = ""
			newMsg.ToolCalls = append(newMsg.ToolCalls, tool)
		}

		newRequest.Messages = append(newRequest.Messages, newMsg)
	}
	return hash.Seed(newRequest)
}

func (c *Client) fromCache(ctx context.Context, messageRequest types.CompletionRequest, request openai.ChatCompletionRequest) (result []openai.ChatCompletionStreamResponse, _ bool, _ error) {
	if messageRequest.Cache != nil && !*messageRequest.Cache {
		return nil, false, nil
	}

	cache, found, err := c.cache.Get(ctx, c.cacheKey(request))
	if err != nil {
		return nil, false, err
	} else if !found {
		return nil, false, nil
	}

	gz, err := gzip.NewReader(bytes.NewReader(cache))
	if err != nil {
		return nil, false, err
	}
	return result, true, json.NewDecoder(gz).Decode(&result)
}

func toToolCall(call types.CompletionToolCall) openai.ToolCall {
	return openai.ToolCall{
		Index: call.Index,
		ID:    call.ID,
		Type:  openai.ToolType(call.Type),
		Function: openai.FunctionCall{
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
		},
	}
}

func toMessages(ctx context.Context, cache *cache.Client, request types.CompletionRequest) (result []openai.ChatCompletionMessage, err error) {
	for _, message := range request.Messages {
		if request.Vision {
			message, err = vision.ToVisionMessage(ctx, cache, message)
			if err != nil {
				return nil, err
			}
		}

		chatMessage := openai.ChatCompletionMessage{
			Role: string(message.Role),
		}

		if message.ToolCall != nil {
			chatMessage.ToolCallID = message.ToolCall.ID
		}

		for _, content := range message.Content {
			if content.ToolCall != nil {
				chatMessage.ToolCalls = append(chatMessage.ToolCalls, toToolCall(*content.ToolCall))
			}
			if content.Image != nil {
				url, err := vision.ImageToURL(ctx, cache, request.Vision, *content.Image)
				if err != nil {
					return nil, err
				}
				if request.Vision {
					chatMessage.MultiContent = append(chatMessage.MultiContent, openai.ChatMessagePart{
						Type: openai.ChatMessagePartTypeImageURL,
						ImageURL: &openai.ChatMessageImageURL{
							URL: url,
						},
					})
				} else {
					chatMessage.MultiContent = append(chatMessage.MultiContent, openai.ChatMessagePart{
						Type: openai.ChatMessagePartTypeText,
						Text: fmt.Sprintf("Image URL %s", url),
					})
				}
			}
			if content.Text != "" {
				chatMessage.MultiContent = append(chatMessage.MultiContent, openai.ChatMessagePart{
					Type: openai.ChatMessagePartTypeText,
					Text: content.Text,
				})
			}
		}

		if len(chatMessage.MultiContent) == 1 && chatMessage.MultiContent[0].Type == openai.ChatMessagePartTypeText {
			if chatMessage.MultiContent[0].Text == "." || chatMessage.MultiContent[0].Text == "{}" {
				continue
			}
			chatMessage.Content = chatMessage.MultiContent[0].Text
			chatMessage.MultiContent = nil

			if strings.Contains(chatMessage.Content, DefaultPromptParameter) && strings.HasPrefix(chatMessage.Content, "{") {
				data := map[string]any{}
				if err := json.Unmarshal([]byte(chatMessage.Content), &data); err == nil && len(data) == 1 {
					if v, _ := data[DefaultPromptParameter].(string); v != "" {
						chatMessage.Content = v
					}
				}
			}
		}

		result = append(result, chatMessage)
	}
	return
}

type Status struct {
	OpenAITransactionID string
	Request             any
	Response            any
	Cached              bool
	Chunks              any
	PartialResponse     *types.CompletionMessage
}

func (c *Client) Call(ctx context.Context, messageRequest types.CompletionRequest, status chan<- Status) (*types.CompletionMessage, error) {
	msgs, err := toMessages(ctx, c.cache, messageRequest)
	if err != nil {
		return nil, err
	}

	if len(msgs) == 0 {
		return nil, fmt.Errorf("invalid request, no messages to send to OpenAI")
	}

	request := openai.ChatCompletionRequest{
		Model:     messageRequest.Model,
		Messages:  msgs,
		MaxTokens: messageRequest.MaxToken,
	}

	if messageRequest.JSONResponse {
		request.ResponseFormat = &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		}
	}

	if request.MaxTokens == 0 {
		request.MaxTokens = DefaultMaxTokens
	}

	if !messageRequest.Vision {
		for _, tool := range messageRequest.Tools {
			params := tool.Function.Parameters
			if params != nil && params.Type == "object" && params.Properties == nil {
				params.Properties = map[string]types.Property{}
			}
			request.Tools = append(request.Tools, openai.Tool{
				Type: openai.ToolType(tool.Type),
				Function: openai.FunctionDefinition{
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  params,
				},
			})
		}
	}

	id := fmt.Sprint(atomic.AddInt64(&transactionID, 1))
	status <- Status{
		OpenAITransactionID: id,
		Request:             request,
	}

	var cacheResponse bool
	request.Seed = ptr(c.seed(request))
	response, ok, err := c.fromCache(ctx, messageRequest, request)
	if err != nil {
		return nil, err
	} else if !ok {
		response, err = c.call(ctx, request, id, status)
		if err != nil {
			return nil, err
		}
	} else {
		cacheResponse = true
	}

	result := types.CompletionMessage{}
	for _, response := range response {
		result = appendMessage(result, response)
	}

	status <- Status{
		OpenAITransactionID: id,
		Chunks:              response,
		Response:            result,
		Cached:              cacheResponse,
	}

	return &result, nil
}

func appendMessage(msg types.CompletionMessage, response openai.ChatCompletionStreamResponse) types.CompletionMessage {
	if len(response.Choices) == 0 {
		return msg
	}

	delta := response.Choices[0].Delta
	msg.Role = types.CompletionMessageRoleType(override(string(msg.Role), delta.Role))

	for _, tool := range delta.ToolCalls {
		if tool.Index == nil {
			continue
		}
		idx := *tool.Index
		for len(msg.Content)-1 < idx {
			msg.Content = append(msg.Content, types.ContentPart{
				ToolCall: &types.CompletionToolCall{
					Index: ptr(len(msg.Content)),
				},
			})
		}

		tc := msg.Content[idx]
		if tc.ToolCall == nil {
			tc.ToolCall = &types.CompletionToolCall{}
		}
		if tool.Index != nil {
			tc.ToolCall.Index = tool.Index
		}
		tc.ToolCall.ID = override(tc.ToolCall.ID, tool.ID)
		tc.ToolCall.Type = types.CompletionToolType(override(string(tc.ToolCall.Type), string(tool.Type)))
		tc.ToolCall.Function.Name += tool.Function.Name
		tc.ToolCall.Function.Arguments += tool.Function.Arguments

		msg.Content[idx] = tc
	}

	if delta.Content != "" {
		found := false
		for i, content := range msg.Content {
			if content.ToolCall != nil || content.Image != nil {
				continue
			}
			msg.Content[i] = types.ContentPart{
				Text: msg.Content[i].Text + delta.Content,
			}
			found = true
			break
		}
		if !found {
			msg.Content = append(msg.Content, types.ContentPart{
				Text: delta.Content,
			})
		}
	}

	return msg
}

func override(left, right string) string {
	if right != "" {
		return right
	}
	return left
}

func (c *Client) store(ctx context.Context, key string, responses []openai.ChatCompletionStreamResponse) error {
	buf := &bytes.Buffer{}
	gz := gzip.NewWriter(buf)
	err := json.NewEncoder(gz).Encode(responses)
	if err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return c.cache.Store(ctx, key, buf.Bytes())
}

func (c *Client) call(ctx context.Context, request openai.ChatCompletionRequest, transactionID string, partial chan<- Status) (responses []openai.ChatCompletionStreamResponse, _ error) {
	cacheKey := c.cacheKey(request)
	request.Stream = true

	msg := ""
	if len(request.Messages) > 0 {
		msg = request.Messages[len(request.Messages)-1].Content
		if msg != "" {
			msg = "Sent content:\n\n" + msg + "\n"
		}
	}

	partial <- Status{
		OpenAITransactionID: transactionID,
		PartialResponse: &types.CompletionMessage{
			Role:    types.CompletionMessageRoleTypeAssistant,
			Content: types.Text(msg + "Waiting for model response...\n"),
		},
	}

	slog.Debug("calling openai", "message", request.Messages)
	stream, err := c.c.CreateChatCompletionStream(ctx, request)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	var partialMessage types.CompletionMessage
	for {
		response, err := stream.Recv()
		if err == io.EOF {
			return responses, c.store(ctx, cacheKey, responses)
		} else if err != nil {
			return nil, err
		}
		slog.Debug("stream", "content", response.Choices[0].Delta.Content)
		if partial != nil {
			partialMessage = appendMessage(partialMessage, response)
			partial <- Status{
				OpenAITransactionID: transactionID,
				PartialResponse:     &partialMessage,
			}
		}
		responses = append(responses, response)
	}
}

func ptr[T any](v T) *T {
	return &v
}
